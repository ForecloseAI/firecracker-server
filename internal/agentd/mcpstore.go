package agentd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// MCPStore is this machine's registered remote MCP servers.
//
// One file for the whole machine, for the same reason about-the-person.md is
// machine-wide: everyone here works for the same person, and a tool the boss
// has and the researcher does not is a surface nobody can reason about.
type MCPStore struct {
	path string

	mu sync.Mutex
	by map[string]*mcpRecord
}

// LoadMCPStore reads the registered servers from dir, creating an empty set if
// there is not one yet.
//
// A decode failure propagates and stops the daemon, as LoadRoster's does. Every
// write here is atomic, so a corrupt file means a damaged disk -- agents.json
// beside it would be damaged too -- and falling back to an empty store would
// let the next write silently overwrite everything the person registered.
func LoadMCPStore(dir string) (*MCPStore, error) {
	s := &MCPStore{path: filepath.Join(dir, "mcp-servers.json"), by: map[string]*mcpRecord{}}
	buf, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, os.MkdirAll(dir, 0o750)
	}
	if err != nil {
		return nil, err
	}
	var records []*mcpRecord
	if err := json.Unmarshal(buf, &records); err != nil {
		return nil, err
	}
	for _, rec := range records {
		s.by[rec.ID] = rec
	}
	return s, nil
}

// List returns every registered server, ordered by id so a client rendering a
// list on every poll gets a stable order.
func (s *MCPStore) List() []mcpRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// listLocked is List without taking the lock. Caller holds s.mu.
func (s *MCPStore) listLocked() []mcpRecord {
	out := make([]mcpRecord, 0, len(s.by))
	for _, rec := range s.by {
		out = append(out, rec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Enabled returns only the servers whose tools an agent should be offered.
func (s *MCPStore) Enabled() []mcpRecord {
	out := make([]mcpRecord, 0, 4)
	for _, rec := range s.List() {
		if rec.Enabled {
			out = append(out, rec)
		}
	}
	return out
}

// Get returns one server by id.
func (s *MCPStore) Get(id string) (mcpRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.by[id]
	if !ok {
		return mcpRecord{}, false
	}
	return rec.clone(), true
}

// HasURL reports whether a server is already registered at this address, so a
// second registration is a conflict rather than two servers whose namespaced
// tools shadow each other.
func (s *MCPStore) HasURL(raw string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.by {
		if rec.URL == raw {
			return true
		}
	}
	return false
}

// Add registers a server, choosing a free id derived from its name.
func (s *MCPStore) Add(rec mcpRecord) (mcpRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.freeIDLocked(rec.Name, rec.URL)
	if err != nil {
		return mcpRecord{}, err
	}
	rec.ID, rec.Enabled, rec.CreatedAt = id, true, time.Now().UTC()
	stored := rec.clone()
	s.by[id] = &stored
	return rec, s.saveLocked()
}

// SetEnabled turns a server on or off, keeping everything else about it.
func (s *MCPStore) SetEnabled(id string, on bool) (mcpRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.by[id]
	if !ok {
		return mcpRecord{}, fmt.Errorf("no server %s", id)
	}
	rec.Enabled = on
	return rec.clone(), s.saveLocked()
}

// Remove forgets a server entirely.
func (s *MCPStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.by[id]; !ok {
		return fmt.Errorf("no server %s", id)
	}
	delete(s.by, id)
	return s.saveLocked()
}

// freeIDLocked derives an id from a name, falling back to the URL's host when
// the name yields nothing usable. Caller holds s.mu.
func (s *MCPStore) freeIDLocked(name, raw string) (string, error) {
	base := slug(name)
	if base == "" {
		base = slug(hostOf(raw))
	}
	if base == "" {
		return "", fmt.Errorf("could not derive an id from %q", name)
	}
	for n := 1; n < 100; n++ {
		id := base
		if n > 1 {
			id = base + "-" + strconv.Itoa(n)
		}
		if _, taken := s.by[id]; !taken {
			return id, nil
		}
	}
	return "", fmt.Errorf("too many servers named %q", base)
}

// saveLocked persists via a temp file and a rename, so a crash mid-write cannot
// leave a half-written file that would lose every registration. Caller holds
// s.mu.
func (s *MCPStore) saveLocked() error {
	records := s.listLocked()
	buf, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// clone deep-copies the mutable fields.
//
// Roster gets away with a shallow struct copy; this carries a map and a slice.
// A shallow copy would hand callers the live Headers map, and redaction -- which
// is the only reason anyone reads a record -- would then erase the person's
// token from the store in memory, with the next save writing the erasure to
// disk permanently.
func (r mcpRecord) clone() mcpRecord {
	out := r
	if r.Headers != nil {
		out.Headers = make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			out.Headers[k] = v
		}
	}
	out.Tools = append([]mcpToolSpec(nil), r.Tools...)
	return out
}

// redact projects a stored record onto the wire shape.
//
// The ONLY constructor of agentapi.MCPServer in this codebase, so there is no
// path by which a secret reaches a client -- guest or /v1. Two things go: the
// header values, and the URL's userinfo and query string.
func redact(rec mcpRecord) agentapi.MCPServer {
	return agentapi.MCPServer{
		ID: rec.ID, Name: rec.Name, URL: safeURL(rec.URL),
		Transport: orDefault(rec.Transport, transportHTTP),
		Enabled:   rec.Enabled, CreatedAt: rec.CreatedAt,
		HeaderKeys: headerKeys(rec.Headers), Tools: usableTools(rec),
		Reachable: rec.ProbeErr == "", Checked: rec.ProbedAt, Error: rec.ProbeErr,
	}
}

// safeURL is a registered URL with its secrets removed.
//
// Several hosted MCP servers pass a token as ?key=..., and the URL is the field
// a client is likeliest to log or render in full. Host and path is what a person
// recognises a server by; the rest is no more inspectable than the header, which
// is the right symmetry.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// headerKeys names the headers that are set, sorted so a poll does not churn.
func headerKeys(h map[string]string) []string {
	if len(h) == 0 {
		return nil
	}
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// usableTools is what this server actually contributes, by model-facing name.
//
// The same filter wrapSpecs applies, so what a person is shown is what their
// agents get -- a tool listed here and missing from the surface would be a
// support question nobody could answer.
func usableTools(rec mcpRecord) []string {
	out := make([]string, 0, len(rec.Tools))
	for _, spec := range rec.Tools {
		if name := modelName(rec.ID, spec.Name); validToolName(name) {
			out = append(out, name)
		}
	}
	return out
}
