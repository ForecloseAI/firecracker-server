package agentd

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"cracked/internal/hoststat"
)

// beat is how often an idle SSE connection emits a comment, so a proxy in
// between does not time the stream out.
const beat = 15 * time.Second

// Server is the agent daemon's HTTP surface.
//
// There is no authentication here, deliberately. In the VM this sits behind the
// control plane's authenticated /vms/{id}/agent/ proxy, which strips the
// Authorization and Cookie headers before forwarding; locally it binds
// loopback. Adding a second auth scheme here would be a second thing to get
// wrong, not a second layer of defence.
type Server struct {
	sup     *Supervisor
	started time.Time

	// seen collapses a retried or double-tapped send. In memory only, like the
	// TypeScript agent's: a restart forgets keys, which is the right trade for
	// a guard against double-taps seconds apart.
	mu   sync.Mutex
	seen map[string]string
	seq  int
}

// NewServer wires a server over a supervisor.
func NewServer(sup *Supervisor) *Server {
	return &Server{sup: sup, started: time.Now(), seen: map[string]string{}}
}

// apiError is the error shape every endpoint uses, matching the control plane
// and the TypeScript agent it will eventually replace.
type apiError struct {
	Error    string `json:"error"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
}

// reply sends a JSON body.
func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// fail sends an error in the standard shape.
func fail(w http.ResponseWriter, status int, code, msg, resource string) {
	reply(w, status, apiError{Error: code, Message: msg, Resource: resource})
}

// nextMessageID mints an id for a queued message, or replays the one a
// previous send with the same idempotency key was given.
func (s *Server) nextMessageID(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		if id, ok := s.seen[key]; ok {
			return id, true
		}
	}
	s.seq++
	id := "m_" + pad(s.seq)
	if key != "" {
		s.seen[key] = id
	}
	return id, false
}

// pad renders a message sequence as at least three digits.
func pad(n int) string {
	s := ""
	for v := n; v > 0; v /= 10 {
		s = string(rune('0'+v%10)) + s
	}
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// memReport is what /debug/memstats returns: the Go heap side of the picture,
// plus the per-agent conversation size, which is the term that grows without
// bound. Process RSS is sampled from outside, since the two diverge.
type memReport struct {
	HeapAllocBytes uint64         `json:"heap_alloc_bytes"`
	HeapSysBytes   uint64         `json:"heap_sys_bytes"`
	Goroutines     int            `json:"goroutines"`
	UptimeSeconds  int64          `json:"uptime_seconds"`
	AgentsTotal    int            `json:"agents_total"`
	AgentsLive     int            `json:"agents_live"`
	Conversations  map[string]int `json:"conversation_bytes"`
	RSSBytes       int64          `json:"rss_bytes"`
}

// selfRSS is the process's resident set size, which is the number that matters
// in a 4 GiB guest: the Go heap is only part of what the kernel is holding.
// Zero wherever there is no /proc, so polling this from a mac degrades to a
// missing number rather than an error.
func selfRSS() int64 {
	rss, err := hoststat.ProcRSSBytes(os.Getpid())
	if err != nil {
		return 0
	}
	return rss
}

// handleMemstats reports memory, per the plan's per-phase tracking.
func (s *Server) handleMemstats(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	statuses := s.sup.List()
	conv := make(map[string]int, len(statuses))
	for _, st := range statuses {
		conv[st.ID] = st.Conversation
	}
	reply(w, http.StatusOK, memReport{
		HeapAllocBytes: m.HeapAlloc, HeapSysBytes: m.HeapSys,
		Goroutines:    runtime.NumGoroutine(),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		AgentsTotal:   len(statuses), AgentsLive: s.sup.LiveCount(),
		Conversations: conv, RSSBytes: selfRSS(),
	})
}
