package cdp

import (
	"context"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// The order is the assertion. dispatchMouseEvent takes VIEWPORT coordinates and
// Chrome here is a real 1920x1080 window, so an element below the fold has a
// box outside it until the page is scrolled. Reading the quads first and
// scrolling afterwards clicks whatever happens to sit at the old coordinates
// and reports success -- and the mouseMoved is what makes a hover-activated
// dropdown exist at the moment the press lands.
func TestClickScrollsThenReadsQuadsThenMovesPressesReleases(t *testing.T) {
	b, rec := fakeBrowser(t, answers(map[string]string{"DOM.getContentQuads": oneQuad}))
	uid := seed(b, "link", 412)
	if _, err := b.Click(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	want := []string{"DOM.scrollIntoViewIfNeeded", "DOM.getContentQuads",
		"Input.dispatchMouseEvent", "Input.dispatchMouseEvent", "Input.dispatchMouseEvent"}
	if got := strings.Join(rec.methods(), ","); got != strings.Join(want, ",") {
		t.Fatalf("sequence = %s\nwant      %s", got, strings.Join(want, ","))
	}
	for i, kind := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p := rec.sent("Input.dispatchMouseEvent", i)
		if p["type"] != kind || p["x"] != 20.0 || p["y"] != 30.0 {
			t.Errorf("event %d = %v, want %s at the quad centre 20,30", i, p, kind)
		}
	}
}

// The worst failure available here, and the reason this has its own function in
// the source. Empty quads mean hidden, zero-sized, or detached because the page
// moved under the snapshot. Falling through to a default coordinate clicks the
// top-left of the viewport -- the logo or the first nav link on most sites -- on
// a DIFFERENT element, and reports success.
func TestClickWithNoQuadsRefusesInsteadOfClickingTheCorner(t *testing.T) {
	b, rec := fakeBrowser(t, answers(map[string]string{"DOM.getContentQuads": `{"quads":[]}`}))
	uid := seed(b, "link", 412)
	_, err := b.Click(context.Background(), uid)
	if err == nil {
		t.Fatal("an element with no box was clicked anyway")
	}
	if !strings.Contains(err.Error(), "take a new one") {
		t.Errorf("error = %q, want it to name staleness as a cause", err)
	}
	if rec.count("Input.dispatchMouseEvent") != 0 {
		t.Error("a mouse event was dispatched for an element with no box")
	}
}

// A link wrapping across two lines reports one quad per fragment and the first
// is often a zero-width sliver, whose centre lands in the margin rather than on
// the text.
func TestCenterOfSkipsDegenerateQuads(t *testing.T) {
	x, y, ok := centerOf([][]float64{
		{10, 20, 10, 20, 10, 40, 10, 40}, // zero width
		{10, 20, 30, 20, 30, 40, 10, 40},
	})
	if !ok || x != 20 || y != 30 {
		t.Errorf("centerOf = %v,%v,%v; want the second quad's centre 20,30", x, y, ok)
	}
	if _, _, ok := centerOf(nil); ok {
		t.Error("no quads reported a usable point")
	}
}

// Without the select the insert appends, so a field holding "old" becomes
// "oldnew" while the tool reports it typed "new".
func TestFillSelectsBeforeInsertingSoOldTextIsReplaced(t *testing.T) {
	b, rec := fakeBrowser(t, answers(map[string]string{
		"Runtime.evaluate": `{"result":{"value":"INPUT|hello"}}`}))
	uid := seed(b, "textbox", 88)
	if _, got, err := b.Fill(context.Background(), uid, "hello"); err != nil || got != "hello" {
		t.Fatalf("Fill = %q, %v", got, err)
	}
	want := "DOM.scrollIntoViewIfNeeded,DOM.focus,Input.dispatchKeyEvent,Input.insertText,Runtime.evaluate"
	if got := strings.Join(rec.methods(), ","); got != want {
		t.Fatalf("sequence = %s\nwant      %s", got, want)
	}
	p := rec.sent("Input.dispatchKeyEvent", 0)
	cmds, _ := p["commands"].([]any)
	if len(cmds) != 1 || cmds[0] != "selectAll" {
		t.Errorf("clear command = %v, want [selectAll]", p["commands"])
	}
}

// Focusing a button and inserting text does nothing at all and reports success.
// The refusal must also cost no CDP, so the model gets its advice immediately.
func TestFillRefusesAButtonWithoutTouchingThePage(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	uid := seed(b, "button", 7)
	_, _, err := b.Fill(context.Background(), uid, "hello")
	if err == nil || !strings.Contains(err.Error(), "use click") {
		t.Fatalf("err = %v, want advice to click instead", err)
	}
	if len(rec.methods()) != 0 {
		t.Errorf("a refused fill still sent %v", rec.methods())
	}
}

// A native <select> is a combobox in the accessibility tree, so it passes the
// role check, and every command in the fill sequence succeeds against it while
// changing nothing. Its options cannot be clicked either -- the popup is browser
// UI, so it has no content quads -- so both routes fail and the only honest
// answer is to say so rather than report a fill that did not happen.
func TestNativeSelectIsRefusedRatherThanReportedAsFilled(t *testing.T) {
	b, _ := fakeBrowser(t, answers(map[string]string{
		"Runtime.evaluate": `{"result":{"value":"SELECT|"}}`}))
	uid := seed(b, "combobox", 91)
	_, _, err := b.Fill(context.Background(), uid, "France")
	if err == nil || !strings.Contains(err.Error(), "dropdown") {
		t.Fatalf("err = %v, want a dropdown refusal", err)
	}
}

// A uid the page has moved past must never reach the wire. Both failures --
// a retired snapshot and a uid that was never issued -- come back as text so
// the model can recover in one step instead of treating the turn as broken.
func TestStaleAndInventedUIDsNeverReachChrome(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	seed(b, "link", 412)
	if _, err := b.Click(context.Background(), "1_999"); err == nil ||
		!strings.Contains(err.Error(), "no element") {
		t.Errorf("invented uid = %v, want a refusal naming it", err)
	}
	b.invalidate()
	if _, err := b.Click(context.Background(), "1_412"); err == nil ||
		!strings.Contains(err.Error(), "page has changed") {
		t.Errorf("stale uid = %v, want the page-changed guidance", err)
	}
	if len(rec.methods()) != 0 {
		t.Errorf("a bad uid still reached Chrome: %v", rec.methods())
	}
}

// The hole the action lock exists for, and it is real with ONE agent: the SDK's
// tool runner calls handlers concurrently within a single turn. Interleaved,
// the quads read for one element and the press dispatched for another put a
// click on coordinates that belong to something else, with nothing reporting an
// error. Each sequence must be contiguous.
func TestConcurrentActionsDoNotInterleaveTheirSequences(t *testing.T) {
	b, rec := fakeBrowser(t, func(ws *websocket.Conn, m message) {
		body := "{}"
		if m.Method == "DOM.getContentQuads" {
			body = oneQuad
		}
		writeJSON(ws, message{ID: m.ID, Result: []byte(body)})
	})
	uid := seed(b, "link", 412)
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() { b.Click(context.Background(), uid); done <- struct{}{} }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	assertContiguous(t, rec.methods())
}

// assertContiguous checks each click's five commands arrived as one run.
func assertContiguous(t *testing.T, got []string) {
	t.Helper()
	want := "DOM.scrollIntoViewIfNeeded,DOM.getContentQuads,Input.dispatchMouseEvent," +
		"Input.dispatchMouseEvent,Input.dispatchMouseEvent"
	for i := 0; i+5 <= len(got); i += 5 {
		if run := strings.Join(got[i:i+5], ","); run != want {
			t.Fatalf("sequence %d interleaved: %s", i/5, run)
		}
	}
}
