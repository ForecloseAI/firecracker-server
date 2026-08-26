package agentd

import (
	"sort"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// Interactions is every agent currently waiting on a person: the raised hands
// of the whole team, in one place.
//
// This is LIVE STATE, not history. A pending interaction holds a channel and a
// blocked goroutine, so it cannot outlive the process -- which is why it is not
// a log. Modelling it as one is what made "whose log does a worker's question go
// in?" an unanswerable question, and it is why the chat page rebuilt these cards
// by replaying an event stream: a card older than the replay window vanished
// from the page while the agent stayed blocked for half an hour.
//
// Any agent registers here directly. The boss is not in the path, and must not
// be: a specialist that needs the person is asking on its own behalf, not
// through its manager.
type Interactions struct {
	mu     sync.Mutex
	raised map[string]agentapi.Raised
	subs   map[chan agentapi.PendingChange]struct{}
}

// NewInteractions builds an empty hub.
func NewInteractions() *Interactions {
	return &Interactions{
		raised: map[string]agentapi.Raised{},
		subs:   map[chan agentapi.PendingChange]struct{}{},
	}
}

// Raise records that an agent is waiting on a person.
func (h *Interactions) Raise(r agentapi.Raised) {
	if h == nil {
		return
	}
	r.Since = time.Now().UTC()
	h.mu.Lock()
	h.raised[r.ID] = r
	h.mu.Unlock()
	h.publish(agentapi.PendingChange{Raised: &r})
}

// Clear drops an interaction that was answered, timed out or was cancelled.
//
// Called on every exit from a wait, including the ones nobody answered: a card
// left behind for a gate that has given up is a button that does nothing, which
// is worse than no button at all.
func (h *Interactions) Clear(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	_, existed := h.raised[id]
	delete(h.raised, id)
	h.mu.Unlock()
	if existed {
		h.publish(agentapi.PendingChange{ClearedID: id})
	}
}

// List returns every raised hand, oldest first, so a client renders them in the
// order they were asked.
func (h *Interactions) List() []agentapi.Raised {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]agentapi.Raised, 0, len(h.raised))
	for _, r := range h.raised {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Subscribe registers a live listener and returns it with its unsubscribe.
func (h *Interactions) Subscribe() (<-chan agentapi.PendingChange, func()) {
	ch := make(chan agentapi.PendingChange, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
	}
}

// publish fans a change out to live listeners.
//
// A slow reader drops frames rather than stalling the agent that is waiting.
// That is safe here in a way it would not be for billing: this hub is queryable,
// so a client that missed a frame re-reads the current set and is correct again.
func (h *Interactions) publish(c agentapi.PendingChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- c:
		default:
		}
	}
}
