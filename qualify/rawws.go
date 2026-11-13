//go:build qualify

package qualify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"github.com/satbooth/satbooth/nostr"
)

// rawConn is a bare relay connection for what the nwc client does not expose: reading the hub's 13194 and
// measuring a response's plaintext. It answers the relay's NIP-42 AUTH challenge with key.
type rawConn struct {
	ws  *websocket.Conn
	key *nostr.SecretKey
	url string
}

func dialRaw(ctx context.Context, url string, key *nostr.SecretKey) (*rawConn, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(nostr.MaxEventJSON + 4096)
	c := &rawConn{ws: ws, key: key, url: url}
	// The relay sends AUTH first; answer it and wait for the OK.
	parts, err := c.read(ctx)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	var label, challenge string
	if json.Unmarshal(parts[0], &label) != nil {
		label = ""
	}
	if label != "AUTH" || len(parts) != 2 || json.Unmarshal(parts[1], &challenge) != nil {
		ws.CloseNow()
		return nil, fmt.Errorf("expected an AUTH challenge, got %s", shown(label, "AUTH", "EVENT", "OK", "NOTICE", "CLOSED", "EOSE"))
	}
	ae, err := nostr.NewAuth(key, url, challenge, time.Now())
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	if err := c.sendEventFrame("AUTH", ae); err != nil {
		ws.CloseNow()
		return nil, err
	}
	for {
		parts, err := c.read(ctx)
		if err != nil {
			ws.CloseNow()
			return nil, err
		}
		label = ""
		_ = json.Unmarshal(parts[0], &label)
		if label != "OK" {
			continue
		}
		var ok bool
		if len(parts) >= 3 {
			_ = json.Unmarshal(parts[2], &ok)
		}
		if !ok {
			ws.CloseNow()
			return nil, fmt.Errorf("AUTH refused")
		}
		return c, nil
	}
}

func (c *rawConn) close() { _ = c.ws.CloseNow() }

func (c *rawConn) read(ctx context.Context) ([]json.RawMessage, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil || len(parts) == 0 {
		return nil, fmt.Errorf("bad frame from the relay")
	}
	return parts, nil
}

func (c *rawConn) writeText(ctx context.Context, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

func (c *rawConn) sendEventFrame(label string, e *nostr.Event) error {
	eb, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b, _ := json.Marshal([]json.RawMessage{json.RawMessage(`"` + label + `"`), eb})
	return c.writeText(context.Background(), b)
}

// req subscribes with a raw filter JSON object.
func (c *rawConn) req(ctx context.Context, sub, filter string) error {
	return c.writeText(ctx, []byte(`["REQ","`+sub+`",`+filter+`]`))
}

// nextEvent returns the next EVENT of subscription sub, skipping every other frame. A CLOSED for sub is an
// error carrying the relay's reason.
func (c *rawConn) nextEvent(ctx context.Context, sub string) (*nostr.Event, error) {
	for {
		parts, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		var label, id string // fresh per frame: a frame whose first element is not a string has no label
		_ = json.Unmarshal(parts[0], &label)
		if len(parts) >= 2 {
			_ = json.Unmarshal(parts[1], &id)
		}
		switch label {
		case "EVENT":
			if id == sub && len(parts) == 3 {
				return nostr.ParseEvent(parts[2])
			}
		case "CLOSED":
			var msg string
			if len(parts) >= 3 {
				_ = json.Unmarshal(parts[2], &msg)
			}
			return nil, fmt.Errorf("relay closed the subscription (reason of %d bytes)", len(msg))
		}
	}
}

// publish sends an EVENT and waits for the relay's OK.
func (c *rawConn) publish(ctx context.Context, e *nostr.Event) (accepted bool, msg string, err error) {
	if err := c.sendEventFrame("EVENT", e); err != nil {
		return false, "", err
	}
	for {
		parts, err := c.read(ctx)
		if err != nil {
			return false, "", err
		}
		var label, id string
		_ = json.Unmarshal(parts[0], &label)
		if label != "OK" || len(parts) < 3 {
			continue
		}
		_ = json.Unmarshal(parts[1], &id)
		if id != e.ID {
			continue
		}
		_ = json.Unmarshal(parts[2], &accepted)
		if len(parts) >= 4 {
			_ = json.Unmarshal(parts[3], &msg)
		}
		return accepted, msg, nil
	}
}
