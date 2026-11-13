package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/satbooth/satbooth/setup"
)

// A question per field of the answers struct, in the struct's order. The values asked for are put in the same JSON
// the answers file would hold and go through setup.ParsePublicAnswers or setup.ParseHubAnswers, so the file and the
// prompts share one validation. Values that depend on each other are asked as a group: after a group (or a single
// question) the answers so far, with a template of valid values for the questions not yet asked, go through the
// parser; when it refuses and names a field already asked, the reason (the parser's own, which never repeats a
// value) is shown and every question of that field's group is asked again, the other answers kept.

const (
	kString = iota
	kUint
	kList
)

type field struct {
	name, question, def string
	kind                int
	group               string                         // fields with the same group are re-asked together
	when                func(vals map[string]any) bool // a question that is asked only when this holds
}

func asked(name string) func(map[string]any) bool {
	return func(v map[string]any) bool {
		switch x := v[name].(type) {
		case string:
			return x != ""
		case []string:
			return len(x) > 0
		}
		return false
	}
}

var publicFields = []field{
	{name: "network", question: "Network (mainnet, signet or regtest)", group: "net"},
	{name: "host_dir", question: "This host's base directory, as the host sees it (absolute path)"},
	{name: "hub_private_addr", question: "The hub host's address on the private link", group: "net"},
	{name: "public_private_addr", question: "This host's address on the private link", group: "net"},
	{name: "public_addr", question: "The address the checkout page is published on", def: "127.0.0.1"},
	{name: "public_port", question: "The port the checkout page is published on", def: "8080", kind: kUint},
	{name: "checkout_base_url", question: "The checkout page's public URL"},
	{name: "webhook_url", question: "The URL SatBooth sends signed payment webhooks to"},
	{name: "alerts_url", question: "The URL operator alerts are sent to (empty: none)"},
	{name: "descriptor", question: "The watch-only descriptor of the on-chain rail (empty: no on-chain rail)"},
	{name: "monero_address", question: "The Monero primary address (empty: no Monero rail)", group: "monero"},
	{name: "monero_network", question: "The Monero network (the hub bundle supports mainnet only)", def: "mainnet", group: "monero", when: asked("monero_address")},
	{name: "approval_keys", question: "The refund approval public keys, comma-separated (empty: no refunds)", kind: kList, group: "refunds"},
	{name: "payout_budget_sat", question: "The payout connection's budget, in sats", kind: kUint, group: "refunds", when: asked("approval_keys")},
	{name: "payout_max_sat", question: "The most one refund may pay, in sats", kind: kUint, group: "refunds", when: asked("approval_keys")},
	{name: "payout_daily_sat", question: "The most refunds may pay in a UTC day, in sats", kind: kUint, group: "refunds", when: asked("approval_keys")},
	{name: "payout_alert_sat", question: "Alert when the budget's headroom falls below this many sats (0: no alert)", kind: kUint, group: "refunds", when: asked("approval_keys")},
	{name: "currencies", question: "The fiat currencies to price in, comma-separated (empty: none)", kind: kList},
}

var hubFields = []field{
	{name: "network", question: "Network (mainnet, signet or regtest)", group: "net"},
	{name: "hub_private_addr", question: "The hub host's address on the private link", group: "net"},
	{name: "public_private_addr", question: "The public host's address on the private link", group: "net"},
	{name: "host_dir", question: "This host's base directory, as the host sees it (absolute path)"},
	{name: "hub_admin_addr", question: "The address the hub's admin page is published on", def: "127.0.0.1", group: "docker"},
	{name: "hub_subnet", question: "The hub bundle's Docker network", def: "172.30.78.0/25", group: "docker"},
	{name: "hub_ip", question: "The hub's fixed address on it", def: "172.30.78.10", group: "docker"},
	{name: "electrum_subnet", question: "The bundle's internal Docker network", def: "172.30.78.128/25", group: "docker"},
}

// Valid values for the questions not yet asked (documentation addresses, as the project's examples use).
func publicTemplate() map[string]any {
	return map[string]any{"network": "mainnet", "host_dir": "/srv/satbooth", "hub_private_addr": "192.0.2.20",
		"public_private_addr": "192.0.2.10", "public_addr": "127.0.0.1", "public_port": uint64(8080),
		"checkout_base_url": "https://pay.example.com", "webhook_url": "https://hooks.example.com/satbooth"}
}

func hubTemplate() map[string]any {
	return map[string]any{"network": "mainnet", "hub_private_addr": "192.0.2.20", "public_private_addr": "192.0.2.10",
		"host_dir": "/srv/satbooth", "hub_admin_addr": "127.0.0.1", "hub_subnet": "172.30.78.0/25",
		"hub_ip": "172.30.78.10", "electrum_subnet": "172.30.78.128/25"}
}

// check puts the answers so far over the template and runs the real parser. It returns the field a refusal names
// ("" for none) and the refusal.
func check(vals, template map[string]any, parse func([]byte) error) (string, error) {
	t := map[string]any{}
	for k, v := range template {
		t[k] = v
	}
	for k, v := range vals {
		t[k] = v
	}
	// The hub bundle's Monero is mainnet only: an address is checked as a mainnet address at once, before the network
	// question is asked.
	if a, _ := t["monero_address"].(string); a != "" {
		if _, ok := t["monero_network"]; !ok {
			t["monero_network"] = "mainnet"
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", errors.New("cannot encode the answers")
	}
	if err := parse(b); err != nil {
		var ae *setup.AnswerError
		if errors.As(err, &ae) {
			return ae.Field, err
		}
		return "", err
	}
	return "", nil
}

// reason is the refusal as the operator reads it: the field and its rule (never a value), or the plain message.
func reason(err error) string {
	var ae *setup.AnswerError
	if errors.As(err, &ae) {
		return ae.Field + " " + ae.Rule
	}
	return err.Error()
}

// ask runs the questions. preset holds answers given by a flag: those questions are not asked. The end of input is
// an error.
func (p *prompter) ask(fields []field, preset, template map[string]any, parse func([]byte) error) (map[string]any, error) {
	vals := map[string]any{}
	for k, v := range preset {
		vals[k] = v
	}
	// one asks a question. With keep, an empty line keeps the previous answer (never shown: the prompt says [keep]);
	// "-" clears an answer (an empty string, an empty list, 0).
	one := func(f field, keep bool) error {
		if _, ok := preset[f.name]; ok {
			return nil
		}
		for {
			q := f.question
			switch {
			case keep:
				q += " [keep]"
			case f.def != "":
				q += " [" + f.def + "]"
			}
			s, err := p.line(q + ":")
			if err != nil {
				return err
			}
			s = strings.TrimSpace(s)
			if s == "" && keep {
				return nil
			}
			if s == "-" {
				switch f.kind {
				case kString:
					vals[f.name] = ""
				case kUint:
					vals[f.name] = uint64(0)
				case kList:
					vals[f.name] = []string{}
				}
				return nil
			}
			if s == "" {
				s = f.def
			}
			switch f.kind {
			case kString:
				vals[f.name] = s
			case kUint:
				n, perr := strconv.ParseUint(s, 10, 64)
				if perr != nil {
					fmt.Fprintln(p.out, "  Refused: a whole number is required.")
					continue
				}
				vals[f.name] = n
			case kList:
				l := []string{}
				for _, x := range strings.Split(s, ",") {
					if x = strings.TrimSpace(x); x != "" {
						l = append(l, x)
					}
				}
				vals[f.name] = l
			}
			return nil
		}
	}
	// group asks the questions of a group again, in order, honouring the conditions.
	group := func(g string, fs []field) error {
		for _, f := range fs {
			if f.group != g {
				continue
			}
			delete(vals, f.name)
			if f.when != nil && !f.when(vals) {
				continue
			}
			if err := one(f, false); err != nil {
				return err
			}
		}
		return nil
	}
	// everything asks every question again, in order: the way out when the cause of a refusal is not in the field
	// or the group the parser names. Nothing is lost: an empty line keeps an answer, "-" clears one.
	everything := func() error {
		for {
			fmt.Fprintln(p.out, "  Asking every question again: an empty line keeps the answer, - clears an optional one.")
			for _, f := range fields {
				if f.when != nil && !f.when(vals) {
					delete(vals, f.name)
					continue
				}
				_, had := vals[f.name]
				if err := one(f, had); err != nil {
					return err
				}
			}
			_, err := check(vals, template, parse)
			if err == nil {
				return nil
			}
			fmt.Fprintln(p.out, "  Refused:", reason(err)) // a refusal without a field asks everything again too
		}
	}
	done := map[string]bool{}      // the fields asked so far
	groupDone := map[string]bool{} // the groups asked to their last question
	attempts := map[string]int{}   // the refusals of each field so far
	// verify runs the parser on what is known. A refusal that names a field already asked sends that field's group
	// (or the field alone) back to the operator once; a second refusal of the same field means the cause is
	// elsewhere, and every question is asked again. It reports whether that happened.
	verify := func() (bool, error) {
		for {
			who, err := check(vals, template, parse)
			if err == nil || !done[who] {
				return false, nil
			}
			if g := groupOf(fields, who); g != "" && !groupDone[g] { // the group is not finished: wait for its last answer
				return false, nil
			}
			fmt.Fprintln(p.out, "  Refused:", reason(err))
			if attempts[who]++; attempts[who] > 1 {
				return true, everything()
			}
			var f *field
			for i := range fields {
				if fields[i].name == who {
					f = &fields[i]
				}
			}
			if f.group == "" {
				delete(vals, who)
				if err := one(*f, false); err != nil {
					return false, err
				}
			} else if err := group(f.group, fields); err != nil {
				return false, err
			}
		}
	}
	for i, f := range fields {
		done[f.name] = true
		if f.when == nil || f.when(vals) {
			if err := one(f, false); err != nil {
				return nil, err
			}
		}
		last := f.group == ""
		if f.group != "" {
			last = true
			for _, g := range fields[i+1:] {
				if g.group == f.group {
					last = false
				}
			}
		}
		if last {
			if f.group != "" {
				groupDone[f.group] = true
			}
			full, err := verify()
			if err != nil {
				return nil, err
			}
			if full { // every question was asked again and the set is accepted
				return vals, nil
			}
		}
	}
	// The whole set, once more: a refusal here is a mistake across groups: ask everything again.
	if _, err := check(vals, template, parse); err != nil {
		fmt.Fprintln(p.out, "  Refused:", reason(err))
		if err := everything(); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

func isField(fields []field, name string) bool {
	for _, f := range fields {
		if f.name == name {
			return true
		}
	}
	return false
}

func groupOf(fields []field, name string) string {
	for _, f := range fields {
		if f.name == name {
			return f.group
		}
	}
	return ""
}

// publicAnswers reads the answers file, or asks. hostDir ("" when -host-dir was not given) is the answer to host_dir.
func publicAnswers(file, hostDir string, p *prompter) (*setup.PublicAnswers, error) {
	if file != "" {
		b, err := readFile(file, 16<<10+1, false)
		if err != nil {
			return nil, err
		}
		a, err := setup.ParsePublicAnswers(b)
		if err != nil {
			return nil, err
		}
		return a, sameHostDir(hostDir, a.HostDir)
	}
	vals, err := p.ask(publicFields, hostPreset(hostDir), publicTemplate(), func(b []byte) error { _, err := setup.ParsePublicAnswers(b); return err })
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(vals)
	return setup.ParsePublicAnswers(b)
}

func hubAnswers(file, hostDir string, p *prompter) (*setup.HubAnswers, error) {
	if file != "" {
		b, err := readFile(file, 16<<10+1, false)
		if err != nil {
			return nil, err
		}
		a, err := setup.ParseHubAnswers(b)
		if err != nil {
			return nil, err
		}
		return a, sameHostDir(hostDir, a.HostDir)
	}
	vals, err := p.ask(hubFields, hostPreset(hostDir), hubTemplate(), func(b []byte) error { _, err := setup.ParseHubAnswers(b); return err })
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(vals)
	return setup.ParseHubAnswers(b)
}

func hostPreset(hostDir string) map[string]any {
	if hostDir == "" {
		return nil
	}
	return map[string]any{"host_dir": hostDir}
}

// sameHostDir refuses an answers file whose host_dir is not the directory the command was given.
func sameHostDir(given, inFile string) error {
	if given != "" && given != inFile {
		return errors.New("setup: the answers' host_dir is not the directory this command was given (-host-dir)")
	}
	return nil
}
