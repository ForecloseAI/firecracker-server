package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// waitPoll is how often the page is asked. Fast enough that a wait feels
	// immediate, slow enough that the longest one is still only a hundred very
	// cheap calls.
	waitPoll = 250 * time.Millisecond

	// waitDefault and waitCap bound a wait. Past the cap the model should say
	// what it is stuck on rather than hold the turn open.
	waitDefault = 5 * time.Second
	waitCap     = 30 * time.Second
)

// WaitFor blocks until one of the given texts appears on the page.
//
// It takes no action lock, deliberately: a thirty-second wait must not freeze
// the browser for every other agent on the machine. And it polls rather than
// re-snapshotting for two reasons -- the accessibility tree is the 500 KB
// object, and taking one burns a uid generation, so simply waiting would
// silently invalidate every uid the model is holding.
func (b *Browser) WaitFor(ctx context.Context, needles []string, d time.Duration) (string, error) {
	if len(needles) == 0 {
		return "", fmt.Errorf("say what text to wait for")
	}
	limit := capWait(d)
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	got, err := b.poll(ctx, matchExpr(needles))
	if err != nil {
		return "", err
	}
	if got == "" {
		return "", fmt.Errorf("none of %q appeared within %s - take a snapshot "+
			"to see what the page is actually showing", strings.Join(needles, ", "), limit)
	}
	return got, nil
}

// poll asks the page until the predicate matches or the deadline passes.
func (b *Browser) poll(ctx context.Context, expr string) (string, error) {
	for {
		got, err := b.pollOnce(ctx, expr)
		if err != nil {
			return "", err
		}
		if got != "" {
			return got, nil
		}
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(waitPoll):
		}
	}
}

// pollOnce evaluates the predicate once, treating a destroyed execution context
// as "not yet" rather than as a failure.
//
// Mid-navigation Chrome answers "Execution context was destroyed" -- which is
// exactly the moment waiting is most useful, so failing there would break the
// tool at the only time it really matters.
func (b *Browser) pollOnce(ctx context.Context, expr string) (string, error) {
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	err := b.conn.Call(ctx, b.sessionID, "Runtime.evaluate",
		map[string]any{"expression": expr, "returnByValue": true}, &out)
	if err != nil && (isContextGone(err) || ctx.Err() != nil) {
		return "", nil
	}
	return out.Result.Value, err
}

// matchExpr builds a predicate that runs IN the page and returns only whichever
// needle matched.
//
// Shipping document.body.innerText back instead would be 60-120 KB per poll on
// an ordinary article: megabytes of decode-and-discard for a single wait. The
// needles go through json.Marshal, so quoting and injection are impossible.
//
// One frame only. Text inside an iframe will not be found, and the tool
// description says so rather than leaving it to be discovered.
func matchExpr(needles []string) string {
	list, _ := json.Marshal(needles)
	return `(()=>{const n=` + string(list) +
		`;const t=(document.body&&document.body.innerText)||"";` +
		`return n.find(s=>t.includes(s))||""})()`
}

// isContextGone reports a Chrome error meaning the page moved under us.
func isContextGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Execution context was destroyed") ||
		strings.Contains(s, "Cannot find context") ||
		strings.Contains(s, "Cannot find default execution context") ||
		strings.Contains(s, "Inspected target navigated")
}

// capWait keeps a wait inside something one turn can afford.
func capWait(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return waitDefault
	case d > waitCap:
		return waitCap
	}
	return d
}
