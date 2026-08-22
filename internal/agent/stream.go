package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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
func (c *Client) Stream(ctx context.Context, since int, fn func(Event)) error {
	req, err := c.streamRequest(ctx, since)
	if err != nil {
		return err
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent stream: %s", resp.Status)
	}
	return c.consume(ctx, resp, fn)
}

// streamRequest builds the SSE request, resuming from the given event id.
func (c *Client) streamRequest(ctx context.Context, since int) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/session/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if since > 0 {
		req.Header.Set("Last-Event-ID", fmt.Sprint(since))
	}
	return req, nil
}

// consume reads SSE frames until the stream ends or goes quiet.
func (c *Client) consume(ctx context.Context, resp *http.Response, fn func(Event)) error {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	stall := time.AfterFunc(stallAfter, func() { resp.Body.Close() })
	defer stall.Stop()
	for sc.Scan() {
		stall.Reset(stallAfter)
		if ev, ok := decodeLine(sc.Text()); ok {
			fn(ev)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return orStalled(sc.Err())
}

// decodeLine turns one "data:" line into an Event. Other SSE fields and the
// heartbeat comments carry nothing we need: the id is inside the JSON too.
func decodeLine(line string) (Event, bool) {
	payload, found := strings.CutPrefix(line, "data:")
	if !found {
		return Event{}, false
	}
	var ev Event
	if json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev) != nil {
		return Event{}, false
	}
	return ev, ev.ID > 0
}

// orStalled reports a clean end-of-stream as an error too, since the caller
// always wants to reconnect.
func orStalled(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("agent stream ended")
}
