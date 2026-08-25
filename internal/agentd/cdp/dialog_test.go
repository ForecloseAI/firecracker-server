package cdp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// publish drops events for a subscriber that is behind, so the close event is
// not something to rely on. If HandleDialog waited for javascriptDialogClosed
// instead of clearing its own state, one dropped frame would leave every acting
// tool refusing forever on a dialog that had long since gone.
func TestHandleDialogClearsThePendingDialogItself(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	b.setDialog(&Dialog{Type: "confirm", Message: "Are you sure?"})
	if err := b.HandleDialog(context.Background(), true, ""); err != nil {
		t.Fatal(err)
	}
	if _, open := b.PendingDialog(); open {
		t.Error("the dialog is still recorded as open after being handled")
	}
	if p := rec.sent("Page.handleJavaScriptDialog", 0); p == nil || p["accept"] != true {
		t.Errorf("params = %v, want accept true", p)
	}
}

// beforeunload is the one dialog that appears without the agent asking for
// anything: it fires when a page is navigated away from. Leaving it for the
// model would mean navigation wedges on any site that uses one, and the model
// has no way to know that is what happened.
func TestBeforeUnloadIsAnsweredWithoutTheModel(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	b.noteDialog([]byte(`{"type":"beforeunload","message":"Leave site?"}`))
	deadline := time.Now().Add(2 * time.Second)
	for rec.count("Page.handleJavaScriptDialog") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.count("Page.handleJavaScriptDialog") == 0 {
		t.Fatal("a beforeunload was left for the model, which would wedge navigation")
	}
	if _, open := b.PendingDialog(); open {
		t.Error("beforeunload is still blocking after being handled")
	}
}

// A prompt and a confirm need different answers, so the guidance has to carry
// what Chrome actually asked rather than a generic notice.
func TestBlockedNamesTheDialogAndItsMessage(t *testing.T) {
	b, _ := fakeBrowser(t, answers(nil))
	if b.Blocked() != "" {
		t.Fatal("a browser with no dialog reported one")
	}
	b.setDialog(&Dialog{Type: "prompt", Message: "Your name?"})
	got := b.Blocked()
	if !strings.Contains(got, "prompt") || !strings.Contains(got, "Your name?") ||
		!strings.Contains(got, "handle_dialog") {
		t.Errorf("Blocked() = %q, want the type the message and the way out", got)
	}
}

// An ordinary alert must not take the browser away for the life of the VM. The
// acting layer refuses locally on this flag; a real command would park on a
// blocked renderer until the deadline and burn the turn.
func TestABlockedRendererIsDetectedWithoutTouchingIt(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	b.noteDialog([]byte(`{"type":"alert","message":"hi"}`))
	if _, open := b.PendingDialog(); !open {
		t.Fatal("an alert was not recorded")
	}
	if n := len(rec.methods()); n != 0 {
		t.Errorf("noticing an alert cost %d CDP calls, want 0", n)
	}
}
