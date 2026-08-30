package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// Schedule is the wire type, shared with the host so the two cannot drift.
type Schedule = agentapi.Schedule

const (
	// minInterval is the tightest schedule allowed. Every agent can schedule,
	// and compaction bounds what a turn costs but not how many turns there are:
	// a five-minute job is 288 unattended turns a day.
	minInterval = 15 * time.Minute

	// sweepEvery is how often due schedules are looked for. A wall-clock sweep
	// rather than a timer armed at the next firing, because a VM can be Paused
	// and Resumed (internal/vm/control.go) and Go timers run on the monotonic
	// clock, which does not advance while the guest is frozen. A sweep heals
	// itself on the next tick; an armed timer wakes up late by however long the
	// VM slept.
	sweepEvery = 30 * time.Second

	// oneShotGrace is how late a "once on ... at ..." schedule may still run.
	//
	// A repeating job that misses an occurrence has another one behind it, so
	// the sweep spends it and moves on. A one-off has nothing behind it, so it
	// stays due and waits for a free agent instead of being thrown away. It
	// cannot wait forever: a reminder for an 11:00 ticket sale is worth having
	// at 11:20 and is noise by the evening.
	oneShotGrace = time.Hour
)

// ScheduleStore is this machine's persisted schedules, shaped like Roster.
type ScheduleStore struct {
	path string

	mu sync.Mutex
	by map[string]*Schedule
}

// LoadSchedules reads the schedules from dir, creating an empty set if absent.
//
// A decode failure propagates and stops the daemon, as LoadRoster's does. Every
// write here is atomic, so a corrupt file means a damaged disk rather than a
// half-write -- and starting empty would let the next write quietly delete
// everything the person had agreed to.
func LoadSchedules(dir string) (*ScheduleStore, error) {
	s := &ScheduleStore{path: filepath.Join(dir, "schedules.json"), by: map[string]*Schedule{}}
	buf, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, os.MkdirAll(dir, 0o750)
	}
	if err != nil {
		return nil, err
	}
	var records []*Schedule
	if err := json.Unmarshal(buf, &records); err != nil {
		return nil, err
	}
	for _, rec := range records {
		s.by[rec.ID] = rec
	}
	return s, nil
}

// List returns every schedule, ordered by id so a client polling gets a stable
// order.
func (s *ScheduleStore) List() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// listLocked is List without taking the lock. Caller holds s.mu.
func (s *ScheduleStore) listLocked() []Schedule {
	out := make([]Schedule, 0, len(s.by))
	for _, rec := range s.by {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Add stores a schedule under a random id.
//
// Random rather than counted, because a counter has to survive a restart to stay
// unique and this file is a plain array with nowhere to keep one. Numbering from
// the highest id present would hand out an id again as soon as one is deleted,
// and a client still holding the old one would then cancel a schedule it has
// never seen.
func (s *ScheduleStore) Add(sc Schedule) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sc.ID = mintScheduleID(); s.by[sc.ID] != nil; sc.ID = mintScheduleID() {
	}
	sc.Enabled = true
	s.by[sc.ID] = &sc
	return sc, s.saveLocked()
}

// mintScheduleID returns a short random handle.
func mintScheduleID() string { return fmt.Sprintf("sch-%06x", rand.N(1<<24)) }

// Delete drops a schedule, reporting whether there was one and whether the
// removal reached the disk.
//
// A failed write is put back in memory before it is reported. Confirming a
// cancellation that only happened in RAM is the worst of the three outcomes: the
// job resumes at the next restart while list_schedules agrees it is gone, so
// nobody looks again. Restoring it means the error and what the person can see
// say the same thing.
func (s *ScheduleStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.by[id]
	if rec == nil {
		return false, nil
	}
	delete(s.by, id)
	if err := s.saveLocked(); err != nil {
		s.by[id] = rec
		return false, err
	}
	return true, nil
}

// Owner reports which agent a schedule belongs to, and whether it exists.
func (s *ScheduleStore) Owner(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.by[id]
	if !ok {
		return "", false
	}
	return rec.Agent, true
}

// Update edits one schedule in place and persists it. Everything the sweep
// changes -- the next firing, the fire count -- goes through here, so there is
// one writer and the file is never rewritten from a stale copy.
func (s *ScheduleStore) Update(id string, edit func(*Schedule)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.by[id]
	if !ok {
		return
	}
	edit(rec)
	s.saveLocked()
}

// saveLocked writes the store atomically. Caller holds s.mu.
func (s *ScheduleStore) saveLocked() error {
	buf, err := json.MarshalIndent(s.listLocked(), "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// spec is a parsed schedule expression.
type spec struct {
	every  time.Duration // set by "every <dur>"; the clock fields are unused
	once   bool          // set by "once on <date> at <time>"; at holds the instant
	at     time.Time
	hour   int
	minute int
	day    time.Weekday
	weekly bool
	loc    *time.Location
}

// weekdays maps the day names the grammar accepts.
var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseSchedule reads one of the four supported forms:
//
//	every 30m
//	daily at 09:00
//	weekly on mon at 09:00
//	once on 2026-09-02 at 11:00
//
// Deliberately not cron. A parser for cron's ranges, steps and lists is sixty
// lines that has to be right on every field, and a field silently mis-parsed
// fires at the wrong hour forever.
//
// The one-off form is not a convenience. Without it, an agent asked for a single
// event on a named day has no honest way to say so, and the nearest thing the
// grammar accepts is a daily job carrying "do nothing unless today is the 2nd"
// in its own task text. That fires every day forever, spends a turn each time
// deciding not to act, and hangs the whole thing on the model re-reading a date
// correctly on the one morning that matters.
func parseSchedule(expr string, loc *time.Location) (spec, error) {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(expr)))
	switch {
	case len(f) == 2 && f[0] == "every":
		d, err := time.ParseDuration(f[1])
		if err != nil {
			return spec{}, fmt.Errorf("%q is not a duration such as 30m or 6h", f[1])
		}
		if d < minInterval {
			return spec{}, fmt.Errorf("the tightest schedule allowed is %s", minInterval)
		}
		return spec{every: d, loc: loc}, nil
	case len(f) == 3 && f[0] == "daily" && f[1] == "at":
		h, m, err := parseClock(f[2])
		return spec{hour: h, minute: m, loc: loc}, err
	case len(f) == 5 && f[0] == "weekly" && f[1] == "on" && f[3] == "at":
		day, ok := weekdays[f[2]]
		if !ok {
			return spec{}, fmt.Errorf("%q is not a day such as mon or fri", f[2])
		}
		h, m, err := parseClock(f[4])
		return spec{hour: h, minute: m, day: day, weekly: true, loc: loc}, err
	case len(f) == 5 && f[0] == "once" && f[1] == "on" && f[3] == "at":
		d, err := parseDate(f[2])
		if err != nil {
			return spec{}, err
		}
		h, m, err := parseClock(f[4])
		if err != nil {
			return spec{}, err
		}
		// Built in the person's location, like the clock forms, so the date and
		// the time are read together as one wall-clock instant where they are.
		return spec{once: true, loc: loc,
			at: time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)}, nil
	}
	return spec{}, errors.New(`use "every 30m", "daily at 09:00", ` +
		`"weekly on mon at 09:00", or "once on 2026-09-02 at 11:00"`)
}

// parseDate reads an ISO calendar date.
func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date such as 2026-09-02", s)
	}
	return t, nil
}

// parseClock reads a 24-hour HH:MM.
func parseClock(s string) (int, int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a time of day such as 09:00", s)
	}
	return t.Hour(), t.Minute(), nil
}

// next is the first firing strictly after the given time, or the zero time when
// the expression has no firing left -- which only a one-off can reach.
//
// The clock forms are built in the person's own location rather than by adding
// 24h, so a day that is 23 or 25 hours long over a DST change still fires at the
// stated wall-clock time.
func (sp spec) next(after time.Time) time.Time {
	if sp.once {
		if sp.at.After(after) {
			return sp.at
		}
		return time.Time{} // one occurrence, and it is behind us
	}
	if sp.every > 0 {
		return after.Add(sp.every)
	}
	t := after.In(sp.loc)
	at := time.Date(t.Year(), t.Month(), t.Day(), sp.hour, sp.minute, 0, 0, sp.loc)
	for !at.After(after) || (sp.weekly && at.Weekday() != sp.day) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// planSchedule parses an expression and works out when a schedule built from it
// now would first run.
//
// One call rather than parse-then-next at each creation site, because the one-off
// form is where those two steps can disagree: "once on 2026-09-02 at 11:00" is
// perfectly well-formed on the 3rd and still has no run to book, and a caller
// that only checked the parse would store a schedule whose zero NextRunAt reads
// as due on every sweep for the rest of the machine's life.
func planSchedule(expr string, loc *time.Location, now time.Time) (spec, time.Time, error) {
	sp, err := parseSchedule(expr, loc)
	if err != nil {
		return spec{}, time.Time{}, err
	}
	at := sp.next(now)
	if at.IsZero() {
		return spec{}, time.Time{}, fmt.Errorf("%s has already passed",
			sp.at.In(loc).Format("2006-01-02 15:04"))
	}
	return sp, at, nil
}

// zonePath is where the client's timezone is remembered. One file for the
// machine, like about-the-person.md: everyone here works for the same person.
func zonePath(stateDir string) string { return filepath.Join(stateDir, "timezone") }

// RememberZone records where the person is, from whatever the client sent.
//
// An IANA name is preferred over a stamp because they are not equivalent: an
// offset is only true for the instant it was sampled, so a schedule stored from
// +05:30 in July keeps firing at July's offset after a daylight-saving change,
// while a named zone stays correct. Failure is silent -- not knowing the zone
// costs UTC, and it must never fail a person's message.
func RememberZone(stateDir, tz, clientTime string) {
	if _, err := time.LoadLocation(tz); err == nil && tz != "" {
		os.WriteFile(zonePath(stateDir), []byte(tz), 0o640)
		return
	}
	if _, err := time.Parse(time.RFC3339, clientTime); err == nil {
		os.WriteFile(zonePath(stateDir), []byte(clientTime), 0o640)
	}
}

// RememberZone records the zone and, when it has actually changed, moves the
// clock schedules onto it.
//
// The method rather than the function is what the HTTP handlers call, because a
// stored NextRunAt is an absolute instant: a person who books "daily at 09:00"
// in London and then opens the app in Los Angeles would get one firing at the
// old 9am -- 01:00 where they now are -- before the schedule settled onto the
// new zone. The file's own bytes decide whether anything changed, so the usual
// case, the same zone arriving on every message, costs one read and no writes.
func (s *Supervisor) RememberZone(tz, clientTime string) {
	before, _ := os.ReadFile(zonePath(s.stateDir))
	RememberZone(s.stateDir, tz, clientTime)
	after, _ := os.ReadFile(zonePath(s.stateDir))
	if bytes.Equal(before, after) {
		return
	}
	s.rezone(time.Now())
}

// rezone recomputes the next firing of every enabled clock schedule.
//
// "every 30m" is left alone: an interval is not anchored to a wall clock, so
// moving it would only push the next run further out. A one-off does move, for
// the same reason a daily does: "once on 2026-09-02 at 11:00" names a wall-clock
// instant, and the person who wrote it means 11:00 where they are. A schedule
// whose expression no longer parses is left for the sweep, which disables it and
// says why rather than silently skipping it here.
func (s *Supervisor) rezone(now time.Time) {
	loc := loadZone(s.stateDir)
	for _, sc := range s.schedules.List() {
		if !sc.Enabled {
			continue
		}
		sp, err := parseSchedule(sc.Expr, loc)
		if err != nil || sp.every > 0 {
			continue
		}
		// A one-off whose instant has already passed has no next: leave the
		// stored one alone so the sweep can still judge it against oneShotGrace.
		at := sp.next(now)
		if at.IsZero() {
			continue
		}
		s.schedules.Update(sc.ID, func(x *Schedule) { x.NextRunAt = at })
	}
}

// loadZone resolves the remembered timezone, falling back to UTC.
func loadZone(stateDir string) *time.Location {
	buf, err := os.ReadFile(zonePath(stateDir))
	if err != nil {
		return time.UTC
	}
	v := strings.TrimSpace(string(buf))
	if loc, err := time.LoadLocation(v); err == nil {
		return loc
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Location()
	}
	return time.UTC
}

// runSchedules sweeps for due schedules until the daemon stops. Started by
// NewSupervisor and tracked by s.wg, so Close waits for it like an agent.
//
// It watches both s.ctx and its own channel: the context is how a signalled
// daemon stops, and the channel is how Close stops it when the caller's context
// outlives the supervisor.
func (s *Supervisor) runSchedules() {
	defer s.wg.Done()
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.sweepDone:
			return
		case now := <-t.C:
			s.sweep(now)
		}
	}
}

// stopSweep ends the sweep, and is safe to call more than once.
func (s *Supervisor) stopSweep() {
	s.sweepOnce.Do(func() { close(s.sweepDone) })
}

// sweep fires every schedule that has come due.
func (s *Supervisor) sweep(now time.Time) {
	for _, sc := range s.schedules.List() {
		if sc.Enabled && !sc.NextRunAt.After(now) {
			s.fire(sc, now)
		}
	}
}

// fire advances a schedule past now and delivers it, unless the agent is busy.
//
// For a repeating job the advance happens FIRST and unconditionally. A skipped
// occurrence that stayed due would come back on the next tick and every tick
// after it, which is exactly the backlog that skipping a busy agent exists to
// avoid.
//
// A one-off is the opposite case and is handled the opposite way. There is no
// occurrence behind it to fall back on, so spending it on a busy agent would
// mean the single thing the person asked for never happens at all. It is left
// due and retried on each sweep instead -- bounded by oneShotGrace, because a
// machine that is busy all afternoon should say the reminder was missed rather
// than deliver it at midnight or hold it due forever.
func (s *Supervisor) fire(sc Schedule, now time.Time) {
	sp, err := parseSchedule(sc.Expr, loadZone(s.stateDir))
	if err != nil {
		s.disable(sc, "its schedule no longer parses: "+err.Error())
		return
	}
	switch {
	case !sp.once:
		s.schedules.Update(sc.ID, func(x *Schedule) { x.NextRunAt = sp.next(now) })
	case now.Sub(sc.NextRunAt) > oneShotGrace:
		s.disable(sc, "its one time came and went with nothing free to run it")
		return
	}
	if _, ok := s.roster.Get(sc.Agent); !ok {
		s.disable(sc, "the agent it belonged to is gone")
		return
	}
	if !s.idle(sc.Agent) {
		return // busy with this or with the person's own work; catch it next time
	}
	a, err := s.Get(sc.Agent)
	if err != nil {
		return // no capacity right now, which the next occurrence may well have
	}
	if a.SendScheduled(sc.Name, sc.Task) == nil {
		s.schedules.Update(sc.ID, func(x *Schedule) {
			x.LastFired, x.Fires = now, x.Fires+1
			// A one-off is spent by the run it just had. Turned off rather than
			// deleted, so the person opening the app sees that the thing they
			// asked for did happen instead of finding an empty list.
			x.Enabled = !sp.once
		})
	}
}

// disable stops a schedule that can never run again.
//
// Reported to the journal rather than to the agent's transcript: one of the two
// reasons for landing here is that the agent is gone, and logFor only reaches a
// live one, so the line would vanish in exactly the case worth explaining. What
// the person sees is enabled:false on GET /schedules.
func (s *Supervisor) disable(sc Schedule, why string) {
	s.schedules.Update(sc.ID, func(x *Schedule) { x.Enabled = false })
	log.Printf("agentd: schedule %q (%s) turned off: %s", sc.Name, sc.ID, why)
}

// idle reports whether an agent can take a scheduled message right now.
//
// Read from List, which starts nothing: an agent that is not live is idle by
// definition, and one mid-turn must not have a timer's work queued behind the
// person's own.
func (s *Supervisor) idle(id string) bool {
	for _, st := range s.List() {
		if st.ID == id {
			return st.State == "idle"
		}
	}
	return false
}

// rollForward moves every schedule past the occurrences missed while the VM was
// off, without firing them.
//
// A machine asleep for a week must not wake into a week of backlog. nanoclaw and
// OpenClaw both skip rather than replay, and it is what "if the VM is off it does
// not need to run" means.
func (s *Supervisor) rollForward(now time.Time) {
	for _, sc := range s.schedules.List() {
		if sc.NextRunAt.After(now) {
			continue
		}
		sp, err := parseSchedule(sc.Expr, loadZone(s.stateDir))
		if err != nil {
			continue
		}
		// A one-off is left exactly where it is. Its instant does not repeat, so
		// there is nothing to skip forward to, and moving it would write a zero
		// NextRunAt that reads as due on every sweep from here on. Whether the
		// machine came back soon enough for it to still be worth running is the
		// sweep's call, which is the one place that can turn it off and say why.
		if sp.once {
			continue
		}
		s.schedules.Update(sc.ID, func(x *Schedule) { x.NextRunAt = sp.next(now) })
	}
}
