package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cracked/internal/agent"
)

// heartbeat keeps the SSE connection and any proxy in between from idling out.
const heartbeat = 25 * time.Second

// sendReq is the body of /api/send and friends.
type sendReq struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Option string `json:"option"`
}

// listVMs powers the picker shown when no id is given or one is wrong.
func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.control.List()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fleet unreachable"})
		return
	}
	out := make([]map[string]string, 0, len(vms))
	for _, v := range vms {
		out = append(out, map[string]string{"id": v.ID, "state": string(v.State)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": out})
}

// target reports whether a VM can be chatted with, and why not if it cannot.
// The status distinguishes retry-worthy states from a typo.
func (s *Server) target(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "none"})
		return
	}
	view, err := s.control.VM(id)
	if errors.Is(err, ErrNoVM) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "missing", "id": id})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "unreachable", "id": id})
		return
	}
	writeJSON(w, http.StatusOK, s.targetState(view))
}

// targetState classifies a resolved VM for the page's banner.
func (s *Server) targetState(view vmView) map[string]string {
	if string(view.State) != "running" {
		return map[string]string{"status": "paused", "id": view.ID, "state": string(view.State)}
	}
	h, err := agent.New(view.GuestIP, guestPort).Health()
	if err != nil {
		return map[string]string{"status": "unreachable", "id": view.ID}
	}
	if !h.Ready {
		return map[string]string{"status": "starting", "id": view.ID}
	}
	return map[string]string{"status": "ready", "id": view.ID}
}

// resume unpauses a VM so the conversation can continue.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	if err := s.control.Resume(req.ID); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// stream is the SSE endpoint. EventSource reconnects on its own and resends
// Last-Event-ID, so resume needs no client code.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	// WriteTimeout is an ABSOLUTE deadline and would cut the stream off.
	rc.SetWriteDeadline(time.Time{})
	writeSSEHeaders(w)
	b := s.bridge(id)
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)
	s.pump(r, w, rc, b, ch, since(r))
}

// writeSSEHeaders opens the event stream and sets the client's retry interval.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 2000\n\n")
}

// since reads the resume point from the header EventSource sends, or the query.
func since(r *http.Request) int {
	for _, s := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since")} {
		var n int
		if s != "" && json.Unmarshal([]byte(s), &n) == nil {
			return n
		}
	}
	return 0
}

// pump replays history, then forwards live frames until the client leaves.
func (s *Server) pump(r *http.Request, w http.ResponseWriter, rc *http.ResponseController,
	b *Bridge, ch chan Frame, from int) {
	for _, f := range b.history(from) {
		writeFrame(w, f)
	}
	rc.Flush()
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-ch:
			writeFrame(w, f)
			rc.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": beat\n\n")
			rc.Flush()
		}
	}
}

// writeFrame emits one SSE event, keyed by the guest's event id so that
// Last-Event-ID resume reaches all the way down to the guest's log.
func writeFrame(w http.ResponseWriter, f Frame) {
	if f.ID > 0 {
		fmt.Fprintf(w, "id: %d\n", f.ID)
	}
	fmt.Fprintf(w, "event: frame\ndata: %s\n\n", f.marshal())
}

// send queues a user turn on the agent.
func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	if req.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text required"})
		return
	}
	cl, err := s.guestFor(req.ID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if err := cl.SendMessage(req.Text, idempotencyKey(req.ID, req.Text)); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "true"})
}

// idempotencyKey collapses a double-click into one queued message.
func idempotencyKey(id, text string) string {
	bucket := time.Now().Unix() / 5
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", id, text, bucket))
	return hex.EncodeToString(sum[:16])
}

// interrupt stops the current turn.
func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	cl, err := s.guestFor(req.ID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	cl.Interrupt()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// resolvePending answers an approval or question. The browser names an option;
// the body that reaches the guest is authored here and never by the page.
func (s *Server) resolvePending(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	p, live := s.bridge(req.ID).Pending(r.PathValue("id"))
	if !live {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already resolved"})
		return
	}
	body, known := p.Body(req.Option)
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown option"})
		return
	}
	s.forward(w, req.ID, p.ID, body)
}

// forward posts a resolved decision to the guest.
func (s *Server) forward(w http.ResponseWriter, vmID, pendingID string, body map[string]any) {
	cl, err := s.guestFor(vmID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if err := cl.Resolve(pendingID, body); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// guestFor resolves a VM id to a client, fresh each time because a recreate
// moves the VM to a different slot and a different address.
func (s *Server) guestFor(id string) (*agent.Client, error) {
	view, err := s.control.VM(id)
	if err != nil {
		return nil, err
	}
	return agent.New(view.GuestIP, guestPort), nil
}

// decode reads a JSON body and reports whether it was usable.
func decode(w http.ResponseWriter, r *http.Request) (sendReq, bool) {
	var req sendReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return req, false
	}
	return req, true
}
