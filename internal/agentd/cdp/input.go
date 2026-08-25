package cdp

import (
	"context"
	"fmt"
	"strings"
)

// notFillable are the roles it makes no sense to type into. Anything here is
// refused with advice rather than focused and typed at, which does nothing and
// reports success.
//
// Advisory rather than an allowlist, on purpose: only roles that are
// unambiguously not text are listed, and everything else is handed to DOM.focus
// where Chrome's own "Element is not focusable" is the second line of defence.
// An allowlist would turn every accessibility-role oddity into a dead end the
// model cannot argue its way out of.
var notFillable = map[string]bool{
	"button": true, "link": true, "checkbox": true, "radio": true,
	"switch": true, "menuitem": true, "menuitemcheckbox": true,
	"menuitemradio": true, "tab": true, "option": true, "slider": true,
}

// Click presses an element the way a person would: at the centre of its box,
// with a real move, press and release.
func (b *Browser) Click(ctx context.Context, uid string) (Target, error) {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return Target{}, err
	}
	defer b.unlock()
	t, why := b.Resolve(uid)
	if why != "" {
		return Target{}, fmt.Errorf("%s", why)
	}
	x, y, err := b.point(ctx, t)
	if err != nil {
		return t, err
	}
	return t, b.clickAt(ctx, x, y)
}

// point scrolls an element into view and returns the centre of its box.
//
// Scroll FIRST, then read the quads. Input.dispatchMouseEvent takes viewport
// coordinates and Chrome here is a real 1920x1080 window, so anything below the
// fold has a box outside it; doing these two in the other order clicks the
// wrong place and reports success.
func (b *Browser) point(ctx context.Context, t Target) (float64, float64, error) {
	p := map[string]any{"backendNodeId": t.NodeID}
	if err := b.conn.Call(ctx, b.sessionID, "DOM.scrollIntoViewIfNeeded", p, nil); err != nil {
		return 0, 0, notVisible(t, err)
	}
	var out struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := b.conn.Call(ctx, b.sessionID, "DOM.getContentQuads", p, &out); err != nil {
		return 0, 0, notVisible(t, err)
	}
	x, y, ok := centerOf(out.Quads)
	if !ok {
		return 0, 0, notVisible(t, nil)
	}
	return x, y, nil
}

// notVisible explains an element that has no box, and names the likely cause.
//
// This is the failure that must never fall through to a default coordinate.
// Empty quads mean hidden, zero-sized, or detached because the page moved under
// the snapshot -- and clicking (0,0) instead lands on the logo or the first nav
// link, on a DIFFERENT element, reported as success. It is the only remaining
// route to acting on the wrong thing, which is why it gets its own function.
func notVisible(t Target, err error) error {
	msg := t.Label() + " has no visible box on the page - it may have been " +
		"hidden or removed since the snapshot, so take a new one"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	return fmt.Errorf("%s", msg)
}

// centerOf averages the corners of the first quad that has real area.
//
// Not simply quads[0]: a link wrapping across two lines reports one quad per
// fragment, and the first is often a zero-width sliver whose centre sits in the
// margin rather than on the text.
func centerOf(quads [][]float64) (float64, float64, bool) {
	for _, q := range quads {
		if len(q) != 8 || area(q) < 1 {
			continue
		}
		return (q[0] + q[2] + q[4] + q[6]) / 4, (q[1] + q[3] + q[5] + q[7]) / 4, true
	}
	return 0, 0, false
}

// area is the shoelace area of one quad, sign discarded.
func area(q []float64) float64 {
	s := q[0]*q[3] - q[2]*q[1] + q[2]*q[5] - q[4]*q[3] +
		q[4]*q[7] - q[6]*q[5] + q[6]*q[1] - q[0]*q[7]
	if s < 0 {
		return -s / 2
	}
	return s / 2
}

// clickAt sends the move, press and release that a real click is made of.
//
// The move is not ceremony: it sets Chrome's hover state, which is what makes a
// hover-activated dropdown exist at the moment the press lands.
func (b *Browser) clickAt(ctx context.Context, x, y float64) error {
	if err := b.mouse(ctx, "mouseMoved", x, y, "none", 0, 0); err != nil {
		return err
	}
	if err := b.mouse(ctx, "mousePressed", x, y, "left", 1, 1); err != nil {
		return err
	}
	return b.mouse(ctx, "mouseReleased", x, y, "left", 0, 1)
}

// mouse dispatches one mouse event at a viewport coordinate.
func (b *Browser) mouse(ctx context.Context, kind string, x, y float64,
	button string, buttons, clicks int) error {
	return b.conn.Call(ctx, b.sessionID, "Input.dispatchMouseEvent", map[string]any{
		"type": kind, "x": x, "y": y, "button": button,
		"buttons": buttons, "clickCount": clicks, "modifiers": 0,
	}, nil)
}

// Fill types into a text field, replacing whatever was in it, and reports what
// the field actually reads afterwards.
//
// That readback earns its round trip. A readonly, disabled or length-clamped
// field accepts every command in this sequence and keeps its old value, so
// without it the tool reports success on a field it never changed.
func (b *Browser) Fill(ctx context.Context, uid, value string) (Target, string, error) {
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return Target{}, "", err
	}
	defer b.unlock()
	t, err := b.fillTarget(uid)
	if err != nil {
		return t, "", err
	}
	if err := b.typeInto(ctx, t, value); err != nil {
		return t, "", err
	}
	tag, got, err := b.fieldValue(ctx)
	return t, got, orSelectRefusal(tag, t, err)
}

// fillTarget resolves a uid and refuses the roles you cannot type into.
func (b *Browser) fillTarget(uid string) (Target, error) {
	t, why := b.Resolve(uid)
	if why != "" {
		return Target{}, fmt.Errorf("%s", why)
	}
	if notFillable[t.Role] {
		return t, fmt.Errorf("%s is not something you can type into - use click on it instead", t.Label())
	}
	return t, nil
}

// orSelectRefusal turns a native dropdown into an honest refusal.
//
// A <select> is a combobox in the accessibility tree, so it passes the role
// check, and every command in the fill sequence succeeds against it while
// changing nothing. Its options cannot be clicked either: the popup is browser
// UI rather than page content, so it has no content quads. Both routes fail, so
// the only honest answer is to say so rather than report a fill that did not
// happen.
func orSelectRefusal(tag string, t Target, err error) error {
	if err != nil {
		return err
	}
	if strings.EqualFold(tag, "select") {
		return fmt.Errorf("%s is a dropdown menu and I cannot set those yet - "+
			"tell the person what you needed to choose", t.Label())
	}
	return nil
}

// typeInto focuses a field, selects what is in it, and inserts the value.
//
// commands:["selectAll"] rather than an emulated Ctrl+A or a scripted
// el.select(): one call instead of a resolveNode / callFunctionOn /
// releaseObject dance, and a browser-side editing command, so it does not
// depend on the platform's modifier conventions. insertText then makes the real
// edit through Blink's IME commit path, so beforeinput and input fire with the
// true value and a React or Vue controlled field updates. All three clears were
// tried against the guest's Chrome before this one was chosen.
func (b *Browser) typeInto(ctx context.Context, t Target, value string) error {
	p := map[string]any{"backendNodeId": t.NodeID}
	if err := b.conn.Call(ctx, b.sessionID, "DOM.scrollIntoViewIfNeeded", p, nil); err != nil {
		return notVisible(t, err)
	}
	if err := b.conn.Call(ctx, b.sessionID, "DOM.focus", p, nil); err != nil {
		return fmt.Errorf("could not put the cursor in %s (%w)", t.Label(), err)
	}
	if err := b.selectAll(ctx); err != nil {
		return err
	}
	return b.conn.Call(ctx, b.sessionID, "Input.insertText",
		map[string]any{"text": value}, nil)
}

// selectAll selects the focused field's contents with Blink's own editing
// command, so the insert that follows replaces rather than appends.
func (b *Browser) selectAll(ctx context.Context) error {
	return b.conn.Call(ctx, b.sessionID, "Input.dispatchKeyEvent", map[string]any{
		"type": "keyDown", "key": "a", "code": "KeyA",
		"windowsVirtualKeyCode": 65, "nativeVirtualKeyCode": 65,
		"modifiers": 0, "commands": []string{"selectAll"},
	}, nil)
}

// fieldValue reads back what the focused field holds and what kind it is.
func (b *Browser) fieldValue(ctx context.Context) (string, string, error) {
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	const expr = `(()=>{const e=document.activeElement;if(!e)return "|";` +
		`return (e.tagName||"")+"|"+(e.value!==undefined?e.value:(e.innerText||""))})()`
	err := b.conn.Call(ctx, b.sessionID, "Runtime.evaluate",
		map[string]any{"expression": expr, "returnByValue": true}, &out)
	tag, val, _ := strings.Cut(out.Result.Value, "|")
	return tag, val, err
}
