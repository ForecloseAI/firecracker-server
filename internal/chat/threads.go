package chat

import (
	"errors"
	"net/http"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
)

// historyCap bounds one thread. There is no pagination in v1, so beyond this the
// oldest lines are simply not in the app -- which is the right trade when the
// alternative is a browsing agent's day making a phone fetch megabytes.
const historyCap = 200

// Thread is one conversation as the app loads it. The client fetches these per
// agent and in parallel, so one long transcript cannot delay the others.
type Thread struct {
	AgentID  string    `json:"agentId"`
	Messages []Message `json:"messages"`
	// Unread is always 0 until a read watermark exists. Reported honestly rather
	// than invented: a badge that cannot be cleared is worse than no badge.
	Unread      int       `json:"unread"`
	LastMessage string    `json:"lastMessage"`
	LastTime    time.Time `json:"lastTime,omitzero"`
}

// getThread returns one agent's conversation.
//
// This starts nothing: the guest serves a poll straight off disk, which is what
// makes the client's parallel load over the whole roster cheap.
func (s *Server) getThread(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(r.Context(), s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	id := r.PathValue("id")
	events, _, err := cl.EventsSince(id, 0)
	if err != nil {
		threadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildThread(id, events, s.handoffURL(user)))
}

// threadError separates a missing agent from an unreachable machine, so a client
// knows whether retrying is worth anything.
func threadError(w http.ResponseWriter, err error) {
	if isNotFound(err) {
		fail(w, http.StatusNotFound, "no such agent")
		return
	}
	fail(w, http.StatusBadGateway, err.Error())
}

// handoffURL mints a link to the person's own machine screen, or "" when the
// VNC gateway is not wired up. Minted per request because the capability is
// short-lived: one stored on a card would be stale by the time it was tapped.
func (s *Server) handoffURL(user string) string {
	if s.caps == nil {
		return ""
	}
	return s.caps.Mint(machineFor(user))
}

// buildThread projects a log into the conversation the app renders.
func buildThread(id string, events []agentapi.Event, handoff string) Thread {
	msgs := make([]Message, 0, len(events))
	for _, ev := range events {
		m, ok := projectMessage(ev)
		if !ok {
			continue
		}
		if ev.Kind == "handoff" {
			m.URL = handoff // a login has to happen on the machine's own screen
		}
		msgs = append(msgs, m)
	}
	msgs = applyDecisions(msgs, events)
	if len(msgs) > historyCap {
		msgs = msgs[len(msgs)-historyCap:]
	}
	return withPreview(Thread{AgentID: id, Messages: msgs})
}

// withPreview fills the list-screen fields from the final line.
func withPreview(t Thread) Thread {
	if n := len(t.Messages); n > 0 {
		t.LastMessage = previewOf(t.Messages[n-1])
		t.LastTime = t.Messages[n-1].Time
	}
	return t
}

// previewOf is what the thread list shows for one message.
//
// An attachment sent without a note carries no text at all, and a blank line on
// a screen whose whole job is saying what happened reads as nothing having
// happened. The stand-in is made HERE and never written into Message.Text: that
// is the caption the bubble renders, and words the agent did not say do not
// belong in the transcript.
func previewOf(m Message) string {
	switch {
	case m.Text != "":
		return m.Text
	case m.Attachment == nil:
		return ""
	case m.Attachment.Kind == kindImage:
		// Not the file name: it is "0007-screen.png" and says nothing.
		return "Sent a screenshot"
	default:
		return "Sent " + m.Attachment.Name
	}
}

// isNotFound reports whether the guest said the agent does not exist.
func isNotFound(err error) bool {
	var se *agent.StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}
