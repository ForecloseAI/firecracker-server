package cdp

import (
	"context"
	"fmt"
	"strings"
)

// Modifier bits, as Chrome numbers them.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

// keyDef is one key's wire shape.
type keyDef struct {
	Key      string
	Code     string
	VK       int
	Location int
	Text     string
}

// keyTable is transcribed from the key table Chrome's own tooling ships, not
// recalled. The Text field on Enter is the whole reason forms submit -- see
// keyType. A var rather than a function because it is a table and splitting it
// to satisfy a line count would stop it reading as one.
var keyTable = map[string]keyDef{
	"Enter":       {Key: "Enter", Code: "Enter", VK: 13, Text: "\r"},
	"Tab":         {Key: "Tab", Code: "Tab", VK: 9},
	"Escape":      {Key: "Escape", Code: "Escape", VK: 27},
	"Backspace":   {Key: "Backspace", Code: "Backspace", VK: 8},
	"Delete":      {Key: "Delete", Code: "Delete", VK: 46},
	"ArrowUp":     {Key: "ArrowUp", Code: "ArrowUp", VK: 38},
	"ArrowDown":   {Key: "ArrowDown", Code: "ArrowDown", VK: 40},
	"ArrowLeft":   {Key: "ArrowLeft", Code: "ArrowLeft", VK: 37},
	"ArrowRight":  {Key: "ArrowRight", Code: "ArrowRight", VK: 39},
	"Home":        {Key: "Home", Code: "Home", VK: 36},
	"End":         {Key: "End", Code: "End", VK: 35},
	"PageUp":      {Key: "PageUp", Code: "PageUp", VK: 33},
	"PageDown":    {Key: "PageDown", Code: "PageDown", VK: 34},
	"Space":       {Key: " ", Code: "Space", VK: 32, Text: " "},
	"ControlLeft": {Key: "Control", Code: "ControlLeft", VK: 17, Location: 1},
	"ShiftLeft":   {Key: "Shift", Code: "ShiftLeft", VK: 16, Location: 1},
	"AltLeft":     {Key: "Alt", Code: "AltLeft", VK: 18, Location: 1},
	"MetaLeft":    {Key: "Meta", Code: "MetaLeft", VK: 91, Location: 1},
}

// modifierKeys maps each modifier bit to the physical key that produces it.
var modifierKeys = []struct {
	bit int
	def keyDef
}{
	{modAlt, keyTable["AltLeft"]},
	{modCtrl, keyTable["ControlLeft"]},
	{modMeta, keyTable["MetaLeft"]},
	{modShift, keyTable["ShiftLeft"]},
}

// PressKey sends a key or a combination to whatever has focus.
func (b *Browser) PressKey(ctx context.Context, combo string) error {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return err
	}
	defer b.unlock()
	k, mods, err := parseCombo(combo)
	if err != nil {
		return err
	}
	release, err := b.holdModifiers(ctx, mods)
	defer release()
	if err != nil {
		return err
	}
	return b.pressOnce(ctx, k, mods)
}

// holdModifiers presses each modifier and returns the function that lets go.
//
// The caller MUST defer the release before checking the error. If the inner
// press fails while Control is still down, Chrome goes on believing Control is
// held and every later click becomes a ctrl-click, which opens a new tab -- so
// the agent ends up acting on a page it never chose, and nothing reports an
// error. It corrupts every subsequent action rather than the current one, which
// makes it the hardest failure here to diagnose after the fact.
func (b *Browser) holdModifiers(ctx context.Context, mods int) (func(), error) {
	var held []keyDef
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			b.keyEvent(ctx, "keyUp", held[i], 0)
		}
	}
	for _, m := range modifierKeys {
		if mods&m.bit == 0 {
			continue
		}
		if err := b.keyEvent(ctx, "rawKeyDown", m.def, mods); err != nil {
			return release, err
		}
		held = append(held, m.def)
	}
	return release, nil
}

// pressOnce sends the down and the up for one key.
func (b *Browser) pressOnce(ctx context.Context, k keyDef, mods int) error {
	kind, _ := keyType(k, mods)
	if err := b.keyEvent(ctx, kind, k, mods); err != nil {
		return err
	}
	return b.keyEvent(ctx, "keyUp", k, mods)
}

// keyType decides the event kind and its text, which is the rule that makes a
// form submit or silently not.
//
// Chrome emits no keypress for a rawKeyDown, and HTML's implicit form
// submission hangs off the char event. So an Enter dispatched as rawKeyDown --
// or carrying "\n" instead of "\r" -- does nothing at all while every command
// in the sequence reports success. Modifiers other than Shift suppress the
// text, exactly as a real keyboard does.
func keyType(k keyDef, mods int) (string, string) {
	text := k.Text
	if text == "" && len([]rune(k.Key)) == 1 {
		text = k.Key
	}
	if mods&^modShift != 0 || text == "" {
		return "rawKeyDown", ""
	}
	return "keyDown", text
}

// keyEvent dispatches one key event.
func (b *Browser) keyEvent(ctx context.Context, kind string, k keyDef, mods int) error {
	p := map[string]any{
		"type": kind, "key": k.Key, "code": k.Code,
		"windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK,
		"modifiers": mods,
	}
	if k.Location != 0 {
		p["location"] = k.Location
	}
	if _, text := keyType(k, mods); kind == "keyDown" && text != "" {
		p["text"], p["unmodifiedText"] = text, text
	}
	return b.conn.Call(ctx, b.sessionID, "Input.dispatchKeyEvent", p, nil)
}

// parseCombo splits "Control+Shift+R" into its key and its modifier mask.
//
// The trailing-empty-token check is what makes "Control++" mean the literal
// plus key rather than an empty one.
func parseCombo(s string) (keyDef, int, error) {
	parts := strings.Split(s, "+")
	if n := len(parts); n > 1 && parts[n-1] == "" {
		parts = append(parts[:n-2:n-2], "+")
	}
	mods, err := maskOf(parts[:len(parts)-1])
	if err != nil {
		return keyDef{}, 0, err
	}
	name := parts[len(parts)-1]
	k, ok := lookupKey(name)
	if !ok {
		return keyDef{}, 0, fmt.Errorf("I do not know a key called %q", name)
	}
	return k, mods, nil
}

// maskOf turns the leading parts of a combination into a modifier mask.
func maskOf(names []string) (int, error) {
	mods := 0
	for _, n := range names {
		bit := modifierBit(n)
		if bit == 0 {
			return 0, fmt.Errorf("%q is not a modifier - use Control Shift Alt or Meta", n)
		}
		mods |= bit
	}
	return mods, nil
}

// modifierBit is the mask one modifier name contributes.
func modifierBit(name string) int {
	switch name {
	case "Alt", "Option":
		return modAlt
	case "Control", "Ctrl":
		return modCtrl
	case "Meta", "Command", "Cmd":
		return modMeta
	case "Shift":
		return modShift
	}
	return 0
}

// lookupKey finds a named key, falling back to any single printable character.
func lookupKey(name string) (keyDef, bool) {
	if k, ok := keyTable[name]; ok {
		return k, true
	}
	if r := []rune(name); len(r) == 1 {
		return keyDef{Key: name, Code: codeFor(r[0]), VK: vkFor(r[0])}, true
	}
	return keyDef{}, false
}

// codeFor names the physical key a character sits on, which is what page
// shortcut handlers actually match against.
func codeFor(r rune) string {
	switch {
	case r >= 'a' && r <= 'z':
		return "Key" + strings.ToUpper(string(r))
	case r >= 'A' && r <= 'Z':
		return "Key" + string(r)
	case r >= '0' && r <= '9':
		return "Digit" + string(r)
	}
	return ""
}

// vkFor is the Windows virtual key code a character reports.
func vkFor(r rune) int {
	switch {
	case r >= 'a' && r <= 'z':
		return int(r - 'a' + 'A')
	case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return int(r)
	}
	return 0
}
