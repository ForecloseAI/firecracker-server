package cdp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Dialog is a JavaScript dialog Chrome is showing.
//
// While one is open the renderer runs a nested message loop, so everything that
// needs the main thread -- getFullAXTree, every DOM command, every input event
// -- stops completing until it is handled. A single alert() would otherwise
// wedge the browser for the life of the VM, for the person as well as the
// agent. chrome-devtools-mcp refuses twenty-odd tools outright while one is up,
// for the same reason.
type Dialog struct {
	Type          string // alert, confirm, prompt or beforeunload
	Message       string
	DefaultPrompt string
}

// watchDialogs tracks what Chrome is showing, so the tools can refuse cheaply.
func (b *Browser) watchDialogs() {
	opening, stopOpen := b.conn.Subscribe("Page.javascriptDialogOpening")
	closed, stopClosed := b.conn.Subscribe("Page.javascriptDialogClosed")
	defer stopOpen()
	defer stopClosed()
	for {
		select {
		case params, ok := <-opening:
			if !ok {
				return
			}
			b.noteDialog(params)
		case _, ok := <-closed:
			if !ok {
				return
			}
			b.setDialog(nil)
		}
	}
}

// noteDialog records an opening dialog, handling beforeunload itself.
//
// beforeunload is the one dialog that appears without the agent asking for
// anything: it fires when a page is navigated away from. Leaving it for the
// model would mean navigation wedges on any site that uses one, so it is
// accepted here and never reaches the model at all.
func (b *Browser) noteDialog(params json.RawMessage) {
	var ev struct {
		Type          string `json:"type"`
		Message       string `json:"message"`
		DefaultPrompt string `json:"defaultPrompt"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	b.setDialog(&Dialog{Type: ev.Type, Message: ev.Message, DefaultPrompt: ev.DefaultPrompt})
	if ev.Type == "beforeunload" {
		b.HandleDialog(context.Background(), true, "")
	}
}

// setDialog replaces what the browser believes is showing.
func (b *Browser) setDialog(d *Dialog) {
	b.mu.Lock()
	b.dialog = d
	b.mu.Unlock()
}

// PendingDialog reports the dialog holding the page, if there is one.
func (b *Browser) PendingDialog() (Dialog, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dialog == nil {
		return Dialog{}, false
	}
	return *b.dialog, true
}

// HandleDialog accepts or dismisses whatever Chrome is showing.
//
// It takes no action lock, on purpose: if an action wedged while holding one,
// the single tool that can un-wedge the browser would be queued behind the
// wedge. And it clears the local state itself rather than waiting for
// javascriptDialogClosed, because publish drops events for a subscriber that is
// behind -- one dropped close would leave every acting tool refusing forever on
// a dialog that is long gone.
func (b *Browser) HandleDialog(ctx context.Context, accept bool, promptText string) error {
	p := map[string]any{"accept": accept}
	if promptText != "" {
		p["promptText"] = promptText
	}
	if err := b.conn.Call(ctx, b.sessionID, "Page.handleJavaScriptDialog", p, nil); err != nil {
		return err
	}
	b.setDialog(nil)
	return nil
}

// Blocked returns guidance when a dialog is holding the page, "" otherwise.
//
// Every acting tool calls this first. It is a local read with no CDP in it, so
// a blocked renderer costs nothing -- where actually sending the command would
// park it until the deadline and burn the turn.
func (b *Browser) Blocked() string {
	d, ok := b.PendingDialog()
	if !ok {
		return ""
	}
	return fmt.Sprintf("a dialog is open (%s: %q) - use handle_dialog to "+
		"accept or dismiss it before anything else", d.Type, d.Message)
}
