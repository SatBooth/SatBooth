//go:build qualify

// Command uicheck loads the hub's logged-in pages through the UI proxy in a headless Chrome, driven over the Chrome
// DevTools Protocol (the vendored websocket library; no other dependency), and records every request, console message
// and content-policy violation. It fails when a page makes a request to any origin but the proxy's own, or when its
// own recorder is blind: a control that tries three outside origins from the page must be blocked by the policy and
// must show up as violations. Dev-only (scripts/qualify-egress.sh); it is not part of any build without the tag.
// Usage: uicheck -chrome <binary> -base http://127.0.0.1:8080 -password-file <file> -out report.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type cdp struct {
	conn *websocket.Conn
	mu   sync.Mutex
	next int
	wait map[int]chan json.RawMessage
	ev   func(method string, session string, params json.RawMessage)
}

type msg struct {
	ID        int             `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *cdp) readLoop(ctx context.Context) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var m msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.wait[m.ID]
			delete(c.wait, m.ID)
			c.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- json.RawMessage(`{"error":` + fmt.Sprintf("%q", m.Error.Message) + `}`)
				} else {
					ch <- m.Result
				}
			}
			continue
		}
		c.ev(m.Method, m.SessionID, m.Params)
	}
}

func (c *cdp) call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan json.RawMessage, 1)
	c.wait[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params, "sessionId": session})
	if err := c.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(r, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s: %s", method, e.Error)
		}
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type request struct {
	Page    string `json:"page"`
	URL     string `json:"url"`
	Type    string `json:"type,omitempty"`
	Blocked string `json:"blocked,omitempty"`
	Failed  string `json:"failed,omitempty"`
}

type report struct {
	Chrome      string            `json:"chrome"`
	Base        string            `json:"base"`
	Pages       []pageResult      `json:"pages"`
	Requests    []*request        `json:"requests"`
	ThirdParty  []*request        `json:"third_party_sent"`
	Blocked     []*request        `json:"third_party_blocked_by_policy"`
	Console     []string          `json:"console"`
	Violations  []string          `json:"violations"`
	Control     map[string]string `json:"control"`
	ControlSeen []*request        `json:"control_requests"`
	Problems    []string          `json:"problems"`
}

type pageResult struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Final    string `json:"final_url"`
	Title    string `json:"title"`
	TextLen  int    `json:"text_len"`
	Requests int    `json:"requests"`
	LoggedIn bool   `json:"logged_in"`
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func main() {
	chrome := flag.String("chrome", "", "the Chrome binary")
	base := flag.String("base", "", "the proxy's origin, http://host:port")
	pwFile := flag.String("password-file", "", "file holding the hub's unlock password")
	out := flag.String("out", "uicheck.json", "the JSON report")
	flag.Parse()
	pwb, err := os.ReadFile(*pwFile)
	bu, err2 := url.Parse(*base)
	if *chrome == "" || err != nil || err2 != nil || bu.Host == "" {
		fmt.Fprintln(os.Stderr, "usage: uicheck -chrome <binary> -base http://host:port -password-file <file> -out <file>")
		os.Exit(2)
	}
	password := strings.TrimSpace(string(pwb))
	rep := &report{Base: *base, Control: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir, _ := os.MkdirTemp("", "uicheck")
	defer os.RemoveAll(dir)
	cmd := exec.Command(*chrome, "--headless=new", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-extensions",
		"--user-data-dir="+dir, "--remote-debugging-port=0", "about:blank")
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "uicheck: cannot start Chrome:", err)
		os.Exit(1)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var wsURL string
	for i := 0; i < 100 && wsURL == ""; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort")); err == nil {
			if f := strings.Split(strings.TrimSpace(string(b)), "\n"); len(f) >= 2 {
				wsURL = "ws://127.0.0.1:" + f[0] + f[1]
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if wsURL == "" {
		fmt.Fprintln(os.Stderr, "uicheck: Chrome did not open its debugging port")
		os.Exit(1)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "uicheck: cannot connect to Chrome:", err)
		os.Exit(1)
	}
	conn.SetReadLimit(32 << 20)

	var mu sync.Mutex
	phase := "start"
	lastActivity := time.Now()
	loadFired := make(chan struct{}, 8)
	reqURL := map[string]*request{}
	c := &cdp{conn: conn, wait: map[int]chan json.RawMessage{}}
	c.ev = func(method, session string, p json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "Network.requestWillBeSent":
			var e struct {
				RequestID string `json:"requestId"`
				Type      string `json:"type"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			_ = json.Unmarshal(p, &e)
			r := &request{Page: phase, URL: e.Request.URL, Type: e.Type}
			reqURL[e.RequestID] = r
			if phase == "control" {
				rep.ControlSeen = append(rep.ControlSeen, r)
			} else {
				rep.Requests = append(rep.Requests, r)
			}
			lastActivity = time.Now()
		case "Network.webSocketCreated":
			var e struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(p, &e)
			r := &request{Page: phase, URL: e.URL, Type: "WebSocket"}
			if phase == "control" {
				rep.ControlSeen = append(rep.ControlSeen, r)
			} else {
				rep.Requests = append(rep.Requests, r)
			}
			lastActivity = time.Now()
		case "Network.loadingFailed":
			var e struct {
				RequestID     string `json:"requestId"`
				ErrorText     string `json:"errorText"`
				BlockedReason string `json:"blockedReason"`
			}
			_ = json.Unmarshal(p, &e)
			if r := reqURL[e.RequestID]; r != nil {
				r.Failed, r.Blocked = e.ErrorText, e.BlockedReason
			}
			lastActivity = time.Now()
		case "Network.responseReceived", "Network.loadingFinished":
			lastActivity = time.Now()
		case "Page.loadEventFired":
			select {
			case loadFired <- struct{}{}:
			default:
			}
		case "Runtime.consoleAPICalled":
			var e struct {
				Type string `json:"type"`
				Args []struct {
					Value       any    `json:"value"`
					Description string `json:"description"`
				} `json:"args"`
			}
			_ = json.Unmarshal(p, &e)
			var parts []string
			for _, a := range e.Args {
				if a.Value != nil {
					parts = append(parts, fmt.Sprint(a.Value))
				} else {
					parts = append(parts, a.Description)
				}
			}
			t := phase + " console." + e.Type + ": " + clip(strings.Join(parts, " "))
			rep.Console = append(rep.Console, t)
			if strings.Contains(t, "Content Security Policy") || strings.Contains(t, "Refused to") {
				rep.Violations = append(rep.Violations, t)
			}
		case "Log.entryAdded":
			var e struct {
				Entry struct {
					Source, Level, Text, URL string
				} `json:"entry"`
			}
			_ = json.Unmarshal(p, &e)
			t := phase + " log." + e.Entry.Source + "." + e.Entry.Level + ": " + clip(e.Entry.Text)
			rep.Console = append(rep.Console, t)
			if strings.Contains(e.Entry.Text, "Content Security Policy") || strings.Contains(e.Entry.Text, "Refused to") ||
				e.Entry.Source == "security" {
				rep.Violations = append(rep.Violations, t)
			}
		case "Runtime.exceptionThrown":
			rep.Console = append(rep.Console, phase+" exception: "+clip(string(p)))
		}
	}
	go c.readLoop(ctx)

	must := func(session, method string, params any) json.RawMessage {
		r, err := c.call(ctx, session, method, params)
		if err != nil {
			fmt.Fprintln(os.Stderr, "uicheck:", err)
			os.Exit(1)
		}
		return r
	}
	var ver struct {
		Product string `json:"product"`
	}
	_ = json.Unmarshal(must("", "Browser.getVersion", map[string]any{}), &ver)
	rep.Chrome = ver.Product
	var tg struct {
		TargetID string `json:"targetId"`
	}
	_ = json.Unmarshal(must("", "Target.createTarget", map[string]any{"url": "about:blank"}), &tg)
	var at struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(must("", "Target.attachToTarget", map[string]any{"targetId": tg.TargetID, "flatten": true}), &at)
	s := at.SessionID
	for _, d := range []string{"Network.enable", "Page.enable", "Runtime.enable", "Log.enable"} {
		must(s, d, map[string]any{})
	}
	eval := func(expr string, await bool) string {
		r, err := c.call(ctx, s, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": await, "returnByValue": true})
		if err != nil {
			return "error: " + err.Error()
		}
		var e struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
		}
		_ = json.Unmarshal(r, &e)
		return fmt.Sprint(e.Result.Value)
	}
	settle := func(max time.Duration) {
		end := time.Now().Add(max)
		for time.Now().Before(end) {
			time.Sleep(500 * time.Millisecond)
			mu.Lock()
			quiet := time.Since(lastActivity) > 3*time.Second
			mu.Unlock()
			if quiet {
				return
			}
		}
	}
	goTo := func(name, path string) pageResult {
		mu.Lock()
		phase = name
		lastActivity = time.Now()
		n0 := len(rep.Requests)
		mu.Unlock()
		for len(loadFired) > 0 {
			<-loadFired
		}
		must(s, "Page.navigate", map[string]any{"url": *base + path})
		select {
		case <-loadFired:
		case <-time.After(30 * time.Second):
			rep.Problems = append(rep.Problems, name+": no load event in 30 s")
		}
		settle(25 * time.Second)
		pr := pageResult{Name: name, Path: path}
		pr.Final = eval("location.href", false)
		pr.Title = eval("document.title", false)
		fmt.Sscan(eval("document.body ? document.body.innerText.length : 0", false), &pr.TextLen)
		mu.Lock()
		pr.Requests = len(rep.Requests) - n0
		mu.Unlock()
		return pr
	}

	// Log in through the form: the hub's Unlock screen has an input #password and a Login button (frontend/src/screens/Unlock.tsx).
	login := goTo("login", "/unlock")
	rep.Pages = append(rep.Pages, login)
	if eval(`!!document.querySelector('#password')`, false) != "true" {
		rep.Problems = append(rep.Problems, "login: no #password field on /unlock (final URL "+login.Final+")")
	} else {
		mu.Lock()
		phase = "login-submit"
		mu.Unlock()
		eval(`document.querySelector('#password').focus()`, false)
		must(s, "Input.insertText", map[string]any{"text": password})
		// The Login button (type=submit) of the form: a real click, which the React form handles as the user's would.
		if got := eval(`(() => { const b = document.querySelector('form button[type=submit]'); if (!b) return 'no submit button'; b.click(); return 'clicked' })()`, false); got != "clicked" {
			rep.Problems = append(rep.Problems, "login: "+got)
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && strings.Contains(eval("location.pathname", false), "/unlock") {
			time.Sleep(500 * time.Millisecond)
		}
		settle(15 * time.Second)
	}
	for _, p := range [][2]string{{"home", "/home"}, {"channels", "/channels"}, {"receive", "/wallet/receive"},
		{"settings", "/settings"}, {"appstore", "/appstore"}, {"peers", "/peers"}} {
		pr := goTo(p[0], p[1])
		pr.LoggedIn = pr.TextLen > 0 && !strings.Contains(pr.Final, "/unlock") && !strings.Contains(pr.Final, "/welcome") &&
			!strings.Contains(pr.Final, "/start")
		if !pr.LoggedIn {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: not a logged-in page (final URL %s, %d characters of text)", p[0], pr.Final, pr.TextLen))
		}
		rep.Pages = append(rep.Pages, pr)
	}

	// The control: the page itself tries three outside origins. The policy must stop all three, and the recorder must
	// see the violations, or its silence about requests above means nothing.
	mu.Lock()
	phase = "control"
	nv := len(rep.Violations)
	mu.Unlock()
	rep.Control["fetch"] = eval(`fetch('https://example.invalid/control').then(() => 'ALLOWED', e => 'blocked: ' + e.message)`, true)
	rep.Control["websocket"] = eval(`new Promise(res => { try { const w = new WebSocket('wss://example.invalid/control'); w.onopen = () => res('ALLOWED'); w.onerror = () => res('blocked'); } catch (e) { res('blocked: ' + e.message) } })`, true)
	rep.Control["image"] = eval(`new Promise(res => { const i = new Image(); i.onload = () => res('ALLOWED'); i.onerror = () => res('blocked'); i.src = 'https://example.invalid/control.png'; })`, true)
	settle(10 * time.Second)
	mu.Lock()
	newViol := len(rep.Violations) - nv
	phase = "done"
	mu.Unlock()
	for k, v := range rep.Control {
		if strings.HasPrefix(v, "ALLOWED") {
			rep.Problems = append(rep.Problems, "control "+k+" was allowed: the policy does not stop an outside origin")
		}
	}
	if newViol < 3 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("the control caused %d recorded violations, want 3: the recorder may be blind", newViol))
	}

	mu.Lock()
	for _, r := range rep.Requests {
		u, err := url.Parse(r.URL)
		if err != nil {
			rep.ThirdParty = append(rep.ThirdParty, r)
			continue
		}
		switch u.Scheme {
		case "data", "blob", "about":
			continue
		}
		if u.Host != bu.Host {
			// A request the policy stopped never left the browser; it is listed, not counted as a call.
			if r.Blocked == "csp" || strings.Contains(r.Failed, "BLOCKED_BY_CSP") {
				rep.Blocked = append(rep.Blocked, r)
			} else {
				rep.ThirdParty = append(rep.ThirdParty, r)
			}
		}
	}
	if len(rep.ThirdParty) > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d requests to an origin other than %s", len(rep.ThirdParty), bu.Host))
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	mu.Unlock()
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "uicheck:", err)
		os.Exit(1)
	}
	fmt.Printf("uicheck: %s; %d pages, %d requests, %d sent to another origin, %d such requests stopped by the policy, %d violations, %d console lines, %d problems\n",
		rep.Chrome, len(rep.Pages), len(rep.Requests), len(rep.ThirdParty), len(rep.Blocked), len(rep.Violations), len(rep.Console), len(rep.Problems))
	for _, p := range rep.Problems {
		fmt.Println("PROBLEM:", p)
	}
	if len(rep.Problems) > 0 {
		os.Exit(1)
	}
}
