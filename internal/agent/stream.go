package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cracked/internal/agentapi"
)

// stallAfter bounds silence on a stream. The guest heartbeats every 15s, so a
// longer gap means a half-open connection to a wedged guest -- invisible at the
// TCP layer, so we detect it here and force a reconnect.
const stallAfter = 45 * time.Second

// streamClient is deliberately NOT the package's shared client: that one has a
// 2s whole-request timeout, which would cut every stream off after two seconds.
var streamClient = &http.Client{Timeout: 0}

// Stream consumes the guest's SSE event log from after id, calling fn for each
// event. It returns when ctx ends, the guest closes, or the stream stalls.
func (c *Client) Stream(ctx context.Context, agentID string, since int, fn func(agentapi.Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agents/"+agentID+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if since > 0 {
		req.Header.Set("Last-Event-ID", fmt.Sprint(since))
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent stream: %s", resp.Status)
	}
	return scanSSE(ctx, resp, func(payload string) {
		if ev, ok := decodeLine(payload); ok {
			fn(ev)
		}
	})
}

// scanSSE hands every "data:" payload to deliver until the stream ends or goes
// quiet. Shared by both streams so the stall detector -- the only thing that
// notices a half-open connection to a wedged guest -- has one implementation.
func scanSSE(ctx context.Context, resp *http.Response, deliver func(payload string)) error {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	stall := time.AfterFunc(stallAfter, func() { resp.Body.Close() })
	defer stall.Stop()
	for sc.Scan() {
		stall.Reset(stallAfter)
		if payload, found := strings.CutPrefix(sc.Text(), "data:"); found {
			deliver(strings.TrimSpace(payload))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// A clean end-of-stream is an error too: the caller always wants to reconnect.
	return fmt.Errorf("agent stream ended")
}

// decodeLine turns one payload into an Event. The id is inside the JSON, so the
// SSE "id:" line carries nothing this needs.
func decodeLine(payload string) (agentapi.Event, bool) {
	var ev agentapi.Event
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return agentapi.Event{}, false
	}
	return ev, ev.ID > 0
}

// StreamPending consumes the machine's raised hands: every agent currently
// waiting on a person, pushed as they are raised and cleared.
//
// There is no resume cursor and there should not be. Pending state is live, not
// a log -- the guest sends the whole current set on connect, so a reconnect is
// always correct and a dropped frame heals itself.
func (c *Client) StreamPending(ctx context.Context, fn func(agentapi.PendingChange)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/pending/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent pending stream: %s", resp.Status)
	}
	return scanSSE(ctx, resp, func(payload string) { deliverChange(payload, fn) })
}

// deliverChange decodes one hub change and passes it on if it says anything.
//
// The event NAME is deliberately ignored: PendingChange is self-describing, so
// there is no second encoding of "raised or cleared" to keep in agreement.
func deliverChange(payload string, fn func(agentapi.PendingChange)) {
	var c agentapi.PendingChange
	if json.Unmarshal([]byte(payload), &c) != nil {
		return
	}
	if c.Raised != nil || c.ClearedID != "" {
		fn(c)
	}
}
