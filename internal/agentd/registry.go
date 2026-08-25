package agentd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// BossID is the agent the person talks to first. Created on first boot, and
// the only one that cannot be deleted: something has to be accountable for the
// whole result, and a machine with no boss has nobody to ask.
const BossID = agentapi.BossID

// validID is the same shape the control plane already requires of a VM id, so
// agent ids are safe in a path and in a URL without further escaping.
var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Roster is the persisted set of agents on this machine.
type Roster struct {
	path string

	mu sync.Mutex
	by map[string]*Record
}

// LoadRoster reads the roster from dir, creating an empty one if absent.
func LoadRoster(dir string) (*Roster, error) {
	r := &Roster{path: filepath.Join(dir, "agents.json"), by: map[string]*Record{}}
	buf, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return r, os.MkdirAll(dir, 0o750)
	}
	if err != nil {
		return nil, err
	}
	var records []*Record
	if err := json.Unmarshal(buf, &records); err != nil {
		return nil, err
	}
	for _, rec := range records {
		r.by[rec.ID] = rec
	}
	return r, nil
}

// List returns every record, ordered so the boss leads and the rest follow by
// id -- a stable order matters for a client that renders a list every poll.
func (r *Roster) List() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listLocked()
}

// listLocked is List without taking the lock. Caller holds r.mu.
func (r *Roster) listLocked() []Record {
	out := make([]Record, 0, len(r.by))
	for _, rec := range r.by {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == BossID) != (out[j].ID == BossID) {
			return out[i].ID == BossID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns one record by id.
func (r *Roster) Get(id string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.by[id]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

// Add registers a new agent, choosing a free id derived from its name or type.
func (r *Roster) Add(typeKey, name string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, err := r.freeIDLocked(orDefault(name, typeKey))
	if err != nil {
		return Record{}, err
	}
	rec := &Record{ID: id, Name: orDefault(name, typeKey), Type: typeKey, CreatedAt: time.Now().UTC()}
	r.by[id] = rec
	return *rec, r.saveLocked()
}

// EnsureBoss creates the boss record if this machine has never had one.
func (r *Roster) EnsureBoss(typeKey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.by[BossID]; ok {
		return nil
	}
	r.by[BossID] = &Record{ID: BossID, Name: "Boss", Type: typeKey, CreatedAt: time.Now().UTC()}
	return r.saveLocked()
}

// Remove drops an agent. The boss cannot be removed.
func (r *Roster) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == BossID {
		return fmt.Errorf("the boss cannot be deleted")
	}
	if _, ok := r.by[id]; !ok {
		return fmt.Errorf("no agent %s", id)
	}
	delete(r.by, id)
	return r.saveLocked()
}

// SetTask records what an agent is working on, or clears it when task is nil.
func (r *Roster) SetTask(id string, task *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.by[id]
	if !ok {
		return fmt.Errorf("no agent %s", id)
	}
	rec.Task = task
	return r.saveLocked()
}

// freeIDLocked derives an id from a name, adding a suffix until it is unused.
// Caller holds r.mu.
func (r *Roster) freeIDLocked(from string) (string, error) {
	base := slug(from)
	if base == "" {
		return "", fmt.Errorf("could not derive an id from %q", from)
	}
	for n := 1; n < 100; n++ {
		id := base
		if n > 1 {
			id = base + "-" + strconv.Itoa(n)
		}
		if _, taken := r.by[id]; !taken {
			return id, nil
		}
	}
	return "", fmt.Errorf("too many agents named %q", base)
}

// slug reduces a name to a safe id, or "" when nothing usable is left.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case (r == ' ' || r == '-' || r == '_') && b.Len() > 0:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	if !validID.MatchString(out) {
		return ""
	}
	return out
}

// saveLocked persists the roster via a temp file and a rename, so a crash
// mid-write cannot leave a half-written roster that would lose every agent.
// Caller holds r.mu.
func (r *Roster) saveLocked() error {
	buf, err := json.MarshalIndent(r.listLocked(), "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
