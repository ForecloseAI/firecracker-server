package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Wikipedia's article text is 60-120 KB. Shipping document.body.innerText back
// four times a second for a ten-second wait is megabytes of decode-and-discard
// for one tool call, so the predicate has to run in the page and return only
// what matched. The needles go through json.Marshal, so a needle containing a
// quote cannot break the expression.
func TestWaitForEvaluatesThePredicateInThePage(t *testing.T) {
	b, rec := fakeBrowser(t, answers(map[string]string{
		"Runtime.evaluate": `{"result":{"value":"Falco"}}`}))
	got, err := b.WaitFor(context.Background(), []string{`say "hi"`, "Falco"}, time.Second)
	if err != nil || got != "Falco" {
		t.Fatalf("WaitFor = %q, %v", got, err)
	}
	expr, _ := rec.sent("Runtime.evaluate", 0)["expression"].(string)
	if !strings.Contains(expr, `say \"hi\"`) || !strings.Contains(expr, "n.find") {
		t.Errorf("expression = %q, want the needles marshalled and matched in the page", expr)
	}
	if strings.Contains(expr, "return t") || strings.Contains(expr, "return document.body.innerText") {
		t.Error("the expression returns the page text rather than the match")
	}
}

// Mid-navigation Chrome answers "Execution context was destroyed", which is
// exactly the moment waiting is most useful -- you wait BECAUSE the page is
// moving. Treating that as a failure breaks the tool at the only time it
// matters.
func TestWaitForKeepsPollingThroughADestroyedContext(t *testing.T) {
	var calls atomic.Int32
	b, _ := fakeBrowser(t, func(ws *websocket.Conn, m message) {
		if calls.Add(1) == 1 {
			writeJSON(ws, message{ID: m.ID, Error: &wireError{
				Code: -32000, Message: "Execution context was destroyed."}})
			return
		}
		writeJSON(ws, message{ID: m.ID, Result: json.RawMessage(`{"result":{"value":"Falco"}}`)})
	})
	got, err := b.WaitFor(context.Background(), []string{"Falco"}, 3*time.Second)
	if err != nil || got != "Falco" {
		t.Fatalf("WaitFor = %q, %v; want it to survive the navigation", got, err)
	}
}

// A wait that finds nothing must say so in a way the model can act on, and must
// not hold the browser while it waits: WaitFor takes no action lock precisely so
// a thirty-second wait cannot freeze Chrome for every other agent.
func TestWaitForTimesOutWithAdviceAndHoldsNoLock(t *testing.T) {
	b, _ := fakeBrowser(t, answers(map[string]string{
		"Runtime.evaluate": `{"result":{"value":""}}`}))
	done := make(chan error, 1)
	go func() { _, err := b.WaitFor(context.Background(), []string{"nope"}, 300*time.Millisecond); done <- err }()
	select {
	case b.act <- struct{}{}:
		<-b.act
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitFor held the action lock, freezing the browser for every other agent")
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "take a snapshot") {
		t.Errorf("err = %v, want advice on what to do next", err)
	}
}

// A wait with nothing to wait for is a mistake worth naming rather than a
// thirty-second stall.
func TestWaitForNeedsSomethingToWaitFor(t *testing.T) {
	b, _ := fakeBrowser(t, answers(nil))
	if _, err := b.WaitFor(context.Background(), nil, time.Second); err == nil {
		t.Error("an empty wait was accepted")
	}
}
