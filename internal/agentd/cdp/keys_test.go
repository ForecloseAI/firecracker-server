package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// keyEvents returns the key events sent, as "type/key/text" triples.
func keyEvents(r *recorder) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		if m.Method != "Input.dispatchKeyEvent" {
			continue
		}
		var p struct{ Type, Key, Text string }
		json.Unmarshal(m.Params, &p)
		out = append(out, p.Type+"/"+p.Key+"/"+p.Text)
	}
	return out
}

// The single most silent failure in the acting layer. Chrome emits no keypress
// for a rawKeyDown, and HTML's implicit form submission hangs off the char
// event -- so an Enter sent as rawKeyDown, or carrying "\n" instead of "\r",
// submits nothing while every command reports success. A whole class of task
// ("search for X") then fails with every individual step looking fine.
func TestEnterCarriesCarriageReturnSoFormsSubmit(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	if err := b.PressKey(context.Background(), "Enter"); err != nil {
		t.Fatal(err)
	}
	got := keyEvents(rec)
	if len(got) != 2 || got[0] != "keyDown/Enter/\r" {
		t.Fatalf("events = %q, want a keyDown carrying a carriage return", got)
	}
	p := rec.sent("Input.dispatchKeyEvent", 0)
	if p["windowsVirtualKeyCode"] != 13.0 || p["code"] != "Enter" || p["unmodifiedText"] != "\r" {
		t.Errorf("params = %v, want keyCode 13 code Enter and unmodifiedText \\r", p)
	}
}

// A stuck modifier corrupts every LATER action rather than the current one,
// which makes it the hardest failure here to diagnose. With Control still held
// Chrome turns every subsequent click into a ctrl-click, which opens a new tab
// -- so the agent ends up acting on a page it never chose, silently.
func TestModifiersAreReleasedWhenTheKeyPressFails(t *testing.T) {
	b, rec := fakeBrowser(t, func(ws *websocket.Conn, m message) {
		var p struct{ Key string }
		json.Unmarshal(m.Params, &p)
		if p.Key == "r" {
			writeJSON(ws, message{ID: m.ID, Error: &wireError{Code: -32000, Message: "boom"}})
			return
		}
		writeJSON(ws, message{ID: m.ID, Result: json.RawMessage("{}")})
	})
	if err := b.PressKey(context.Background(), "Control+r"); err == nil {
		t.Fatal("expected the inner key press to fail")
	}
	if got := keyEvents(rec); !contains(got, "keyUp/Control/") {
		t.Errorf("events = %q, want Control released despite the failure", got)
	}
}

// contains reports whether a slice holds a string.
func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// A real keyboard produces no character while Control is down, and a page that
// receives text alongside a shortcut sees a keystroke nobody made.
func TestAModifiedKeySendsNoText(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	if err := b.PressKey(context.Background(), "Control+a"); err != nil {
		t.Fatal(err)
	}
	if got := keyEvents(rec); !contains(got, "rawKeyDown/a/") {
		t.Errorf("events = %q, want the modified key sent as rawKeyDown with no text", got)
	}
}

// "Control++" is a real combination and splitting naively parses it as an empty
// key, so the tool rejects something the model was right to ask for.
func TestParseComboHandlesAPlusAsAKey(t *testing.T) {
	k, mods, err := parseCombo("Control++")
	if err != nil || k.Key != "+" || mods != modCtrl {
		t.Errorf("parseCombo(Control++) = %+v, %d, %v", k, mods, err)
	}
	if k, mods, err := parseCombo("Control+Shift+R"); err != nil ||
		k.Code != "KeyR" || mods != modCtrl|modShift {
		t.Errorf("parseCombo(Control+Shift+R) = %+v, %d, %v", k, mods, err)
	}
}

// An unknown key must read as something the model can correct, not as a broken
// tool.
func TestAnUnknownKeyIsExplained(t *testing.T) {
	if _, _, err := parseCombo("Frobnicate"); err == nil ||
		!strings.Contains(err.Error(), "do not know") {
		t.Errorf("err = %v, want a plain explanation", err)
	}
}
