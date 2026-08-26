package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
)

const (
	// tick is how often the gateway asks the guest what changed. A message can
	// therefore take up to this long to appear.
	tick = time.Second
	// beat keeps the socket alive through proxies and a sleeping phone.
	beat = 25 * time.Second
)

// feedFrame is one server-originated update. Type is what the client switches
// on, and it ignores any type it does not know, which is what lets this grow.
type feedFrame struct {
	Type    string   `json:"type"`
	AgentID string   `json:"agentId,omitempty"`
	Message *Message `json:"message,omitempty"`
	Agent   *Agent   `json:"agent,omitempty"`
	// Typing carries no omitempty: the client upserts on this field, and a
	// dropped "false" reads as undefined rather than "stopped working".
	Typing bool `json:"typing"`

	MessageID   string    `json:"messageId,omitempty"`
	Verdict     string    `json:"verdict,omitempty"`
	VerdictTime time.Time `json:"verdictTime,omitzero"`
}

// feed tracks what this connection has already told the client, so a tick only
// sends what moved.
type feed struct {
	cursor   map[string]int    // agent id -> last event id forwarded
	typing   map[string]bool   // agent id -> last typing state sent
	rows     map[string]string // agent id -> last roster row sent, as JSON
	pending  map[string]string // approval id -> the ask's message id
	profiles map[string]agentapi.Profile
	machine  string
	// handoff mints a fresh capability each time it is called.
	handoff func() string
}

// stream is the app's one live connection: every agent on the person's machine,
// over SSE.
//
// SSE rather than a WebSocket because the frames are identical either way and
// this needs no new protocol; the token arrives as a query parameter because a
// browser and React Native both refuse to set headers on a stream.
func (s *Server) streamV1(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // an absolute deadline would cut the stream
	writeSSEHeaders(w)
	rc.Flush()
	f := newFeed(machineFor(user), func() string { return s.handoffURL(user) })
	f.run(r, w, rc, cl)
}

// newFeed starts a connection with nothing sent yet.
func newFeed(machine string, handoff func() string) *feed {
	return &feed{
		cursor: map[string]int{}, typing: map[string]bool{},
		rows: map[string]string{}, pending: map[string]string{},
		profiles: map[string]agentapi.Profile{},
		machine:  machine, handoff: handoff,
	}
}

// run polls until the client leaves or the guest stops answering.
func (f *feed) run(r *http.Request, w io.Writer,
	rc *http.ResponseController, cl *agent.Client) {
	poll := time.NewTicker(tick)
	defer poll.Stop()
	heart := time.NewTicker(beat)
	defer heart.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			fmt.Fprint(w, ": beat\n\n")
			rc.Flush()
		case <-poll.C:
			f.sweep(w, cl)
			rc.Flush()
		}
	}
}

// sweep asks the guest what changed and writes it out. A failed poll is skipped
// rather than fatal: a guest restarting is a blip, not the end of the session.
func (f *feed) sweep(w io.Writer, cl *agent.Client) {
	roster, err := cl.Agents()
	if err != nil {
		return
	}
	f.loadProfiles(cl, roster)
	f.emitRoster(w, roster)
	for _, st := range roster {
		f.emitEvents(w, cl, st)
	}
	f.emitExpiries(w, cl)
}

// loadProfiles fetches the catalog the first time a type appears. Without it an
// agent frame carries no role or capabilities, and since the client UPSERTS on
// this frame, a blank one would wipe what GET /agents told it.
func (f *feed) loadProfiles(cl *agent.Client, roster []agentapi.Status) {
	for _, st := range roster {
		if _, ok := f.profiles[st.Type]; ok {
			continue
		}
		profiles, err := cl.AgentTypes()
		if err != nil {
			return
		}
		for _, p := range profiles {
			f.profiles[p.Key] = p
		}
		return
	}
}

// emitRoster sends an agent frame for every row that changed, which is also how
// a newly activated agent announces itself -- the client upserts by id.
func (f *feed) emitRoster(w io.Writer, roster []agentapi.Status) {
	for _, st := range roster {
		row := projectAgent(st, f.profiles[st.Type], f.machine, true)
		buf, err := json.Marshal(row)
		if err != nil || f.rows[st.ID] == string(buf) {
			continue
		}
		f.rows[st.ID] = string(buf)
		write(w, feedFrame{Type: "agent", Agent: &row})
	}
}

// emitEvents forwards anything new in one agent's log.
func (f *feed) emitEvents(w io.Writer, cl *agent.Client, st agentapi.Status) {
	seen, known := f.cursor[st.ID]
	if !known {
		f.cursor[st.ID] = st.LastEventID // a new connection starts from now
		return
	}
	if st.LastEventID <= seen {
		return
	}
	events, last, err := cl.EventsSince(st.ID, seen)
	if err != nil {
		return
	}
	f.cursor[st.ID] = last
	f.forward(w, st.ID, events)
}

// forward turns one batch of events into frames.
func (f *feed) forward(w io.Writer, id string, events []agentapi.Event) {
	for _, ev := range events {
		if typing, ok := typingOf(ev); ok {
			f.emitTyping(w, id, typing)
			continue
		}
		if ev.Type == "decision" {
			f.emitDecision(w, id, ev)
			continue
		}
		f.emitMessage(w, id, ev)
	}
}

// emitMessage sends one line of conversation.
//
// The person's OWN message is never echoed: the client appended it from the
// send response already, and a second copy would render as a duplicate bubble.
func (f *feed) emitMessage(w io.Writer, id string, ev agentapi.Event) {
	m, ok := projectMessage(ev)
	if !ok || ev.Type == "user" {
		return
	}
	if ev.Kind == "handoff" {
		// Minted now, not when the connection opened. A capability lasts fifteen
		// minutes and a stream stays open for hours, so reusing one handed out a
		// dead link to anyone whose session had been running a while.
		m.URL = f.handoff()
	}
	if m.Kind == kindAsk {
		f.pending[ev.ApprovalID] = m.ID
	}
	write(w, feedFrame{Type: "message", AgentID: id, Message: &m})
}

// emitTyping mirrors the agent's working state, and only on a change: the guest
// suppresses repeats, but a reconnect would otherwise re-announce the same one.
func (f *feed) emitTyping(w io.Writer, id string, typing bool) {
	if was, known := f.typing[id]; known && was == typing {
		return
	}
	f.typing[id] = typing
	write(w, feedFrame{Type: "typing", AgentID: id, Typing: typing})
}

// emitDecision tells the client an ask it is showing has been settled.
func (f *feed) emitDecision(w io.Writer, id string, ev agentapi.Event) {
	msgID, ok := f.pending[ev.ApprovalID]
	if !ok {
		return
	}
	delete(f.pending, ev.ApprovalID)
	write(w, feedFrame{Type: "ask_resolved", AgentID: id, MessageID: msgID,
		Verdict: verdictOf(ev.Decision), VerdictTime: ev.TS})
}

// emitExpiries settles any ask that timed out.
//
// A timeout appends no decision event -- the gate just forgets it -- so the only
// evidence is that it left the hub. Without this the card sits on the phone
// looking live and fails when tapped.
func (f *feed) emitExpiries(w io.Writer, cl *agent.Client) {
	if len(f.pending) == 0 {
		return
	}
	raised, err := cl.Pending()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, r := range raised {
		live[r.ID] = true
	}
	for approvalID, msgID := range f.pending {
		f.expire(w, approvalID, msgID, live)
	}
}

// expire sends one timed-out ask, keyed by the agent its id names.
func (f *feed) expire(w io.Writer, approvalID, msgID string, live map[string]bool) {
	if live[approvalID] {
		return
	}
	delete(f.pending, approvalID)
	agentID, _, _ := strings.Cut(approvalID, ".")
	write(w, feedFrame{Type: "ask_resolved", AgentID: agentID, MessageID: msgID,
		Verdict: "denied", VerdictTime: time.Now().UTC()})
}

// write sends one frame on the default SSE event, which is what an EventSource
// delivers to onmessage with no listener registration.
func write(w io.Writer, f feedFrame) {
	buf, err := json.Marshal(f)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", buf)
}
