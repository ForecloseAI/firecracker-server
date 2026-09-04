package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	slices.SortFunc(out, func(a, b Schedule) int { return strings.Compare(a.ID, b.ID) })
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
	return writeAtomic(s.path, buf)
}

// dayKind is which days a clock-based expression fires on.
type dayKind int

const (
	kindDaily dayKind = iota
	kindWeekly
	kindWeekdays
	kindWeekends
	kindMonthly
)

// clock is a time of day, read in the schedule's own location.
type clock struct{ hour, minute int }

// mins is the time of day as minutes past midnight, for ordering.
func (c clock) mins() int { return c.hour*60 + c.minute }

// spec is a parsed schedule expression.
type spec struct {
	every time.Duration // set by a bare "every <dur>"; the clock fields are unused
	once  bool          // set by either one-off form
	at    time.Time     // the instant of an absolute one-off
	rel   time.Duration // the offset of a relative one-off, resolved at creation

	kind  dayKind      // which days the clock forms fire on
	times []clock      // ascending times of day; at least one for a clock form
	day   time.Weekday // the day kindWeekly fires on
	dom   int          // the day of the month kindMonthly fires on; 0 is the last

	until  time.Time // the last instant that may fire; zero never ends
	pinned bool      // the expression named its own zone, so rezone leaves it
	loc    *time.Location
}

// weekdays maps the day names the grammar accepts.
var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// grammarHelp is what an unrecognised expression is answered with. It lists every
// base form, because this error is the only place a model finds out what it may
// ask for after its first guess misses.
const grammarHelp = `use one of "every 30m", "daily at 09:00", "every weekday at 09:00", ` +
	`"weekly on mon at 09:00", "monthly on the 1st at 09:00", ` +
	`"once on 2026-09-02 at 11:00" or "once in 2h", each optionally followed by ` +
	`"between 09:00 and 17:00", "until 2026-12-31" or "in Asia/Kolkata"`

// parseSchedule reads a schedule expression.
//
// The grammar is a base form plus up to three optional clauses:
//
//	every 30m                     every weekday at 09:00
//	every 2d                      every weekend at 10:00
//	daily at 09:00                monthly on the 1st at 09:00
//	daily at 09:00 and 17:00      monthly on the last day at 18:00
//	weekly on mon at 09:00        once on 2026-09-02 at 11:00
//	                              once in 2h
//
//	... between 09:00 and 17:00   only on "every <dur>": working hours
//	... until 2026-12-31          only on a repeating form: an end date
//	... in Asia/Kolkata           pin to one zone instead of following the person
//
// Deliberately not cron. A parser for cron's ranges, steps and lists is sixty
// lines that has to be right on every field, and a field silently mis-parsed
// fires at the wrong hour forever. The forms here either parse or fail loudly
// with the list above, so a model's near-miss corrects itself inside the turn
// rather than firing at 05:00 for a year.
//
// The one-off forms are not a convenience. Without them, an agent asked for a
// single event on a named day has no honest way to say so, and the nearest thing
// the rest of the grammar accepts is a daily job carrying "do nothing unless
// today is the 2nd" in its own task text. That fires every day forever, spends a
// turn each time deciding not to act, and hangs the whole thing on the model
// re-reading a date correctly on the one morning that matters.
func parseSchedule(expr string, loc *time.Location) (spec, error) {
	p := fields(expr)

	// Clauses are stripped before the base form is matched, so each base stays a
	// fixed shape instead of every combination needing its own case. Zone first:
	// "until" builds a date, and which midnight that is depends on the zone.
	zone, pinned, err := p.takeZone()
	if err != nil {
		return spec{}, err
	}
	if pinned {
		loc = zone
	}
	until, bounded, err := p.takeUntil(loc)
	if err != nil {
		return spec{}, err
	}
	start, end, windowed, err := p.takeWindow()
	if err != nil {
		return spec{}, err
	}

	sp, err := parseBase(p.f, loc)
	if err != nil {
		return spec{}, err
	}
	sp.loc, sp.pinned = loc, pinned

	if bounded {
		if sp.once {
			return spec{}, errors.New(`"until" has no meaning on a one-off, which runs once anyway`)
		}
		sp.until = until
	}
	if windowed {
		if sp.every == 0 {
			return spec{}, errors.New(`"between" only goes with "every <duration>", ` +
				"since the other forms already say what time they run")
		}
		// The window turns the interval into the fixed times of day it actually
		// fires at, which is also what makes it move with the person on a zone
		// change: a bare interval is not anchored to any wall clock, but one held
		// inside working hours is.
		sp.times, sp.every, sp.kind = spread(start, end, sp.every), 0, kindDaily
	}
	if sp.rel > 0 && pinned {
		return spec{}, errors.New("a relative one-off is measured from now, " +
			"so naming a timezone changes nothing")
	}
	return sp, nil
}

// parseBase reads the part of an expression left once the clauses are removed.
func parseBase(f []string, loc *time.Location) (spec, error) {
	if len(f) < 2 {
		return spec{}, errors.New(grammarHelp)
	}
	switch {
	case len(f) == 2 && f[0] == "every":
		d, err := time.ParseDuration(expandDuration(f[1]))
		if err != nil {
			return spec{}, fmt.Errorf("%q is not a duration such as 30m, 6h or 2d", f[1])
		}
		if d < minInterval {
			return spec{}, fmt.Errorf("the tightest schedule allowed is %s", minInterval)
		}
		return spec{every: d}, nil

	case len(f) >= 3 && f[0] == "daily" && f[1] == "at":
		times, err := parseTimes(f[2:])
		return spec{kind: kindDaily, times: times}, err

	case len(f) >= 4 && f[0] == "every" && f[2] == "at" && known(dayWords, f[1]):
		times, err := parseTimes(f[3:])
		return spec{kind: dayWords[f[1]], times: times}, err

	case len(f) >= 5 && f[0] == "weekly" && f[1] == "on" && f[3] == "at":
		day, ok := weekdays[f[2]]
		if !ok {
			return spec{}, fmt.Errorf("%q is not a day such as mon or fri", f[2])
		}
		times, err := parseTimes(f[4:])
		return spec{kind: kindWeekly, day: day, times: times}, err

	case f[0] == "monthly" && f[1] == "on":
		rest := f[2:]
		if len(rest) > 0 && rest[0] == "the" { // "the" is optional
			rest = rest[1:]
		}
		dom, rest, err := parseMonthDay(rest)
		if err != nil {
			return spec{}, err
		}
		if len(rest) < 2 || rest[0] != "at" {
			return spec{}, errors.New(`a monthly schedule needs a time, such as ` +
				`"monthly on the 1st at 09:00"`)
		}
		times, err := parseTimes(rest[1:])
		return spec{kind: kindMonthly, dom: dom, times: times}, err

	// Fixed at five fields rather than a list: "once ... at 11:00 and 15:00"
	// would be two runs, which is not what once means.
	case len(f) == 5 && f[0] == "once" && f[1] == "on" && f[3] == "at":
		d, err := parseDate(f[2])
		if err != nil {
			return spec{}, err
		}
		c, err := parseClock(f[4])
		if err != nil {
			return spec{}, err
		}
		// Built in the person's location, like the clock forms, so the date and
		// the time are read together as one wall-clock instant where they are.
		return spec{once: true, at: time.Date(d.Year(), d.Month(), d.Day(),
			c.hour, c.minute, 0, 0, loc)}, nil

	case len(f) == 3 && f[0] == "once" && f[1] == "in":
		d, err := time.ParseDuration(expandDuration(f[2]))
		if err != nil {
			return spec{}, fmt.Errorf("%q is not a duration such as 20m or 2h", f[2])
		}
		if d <= 0 {
			return spec{}, errors.New("a one-off has to be some time from now")
		}
		// No minInterval here: that bound exists to cap how many unattended turns
		// a standing job can spend, and this one spends exactly one.
		return spec{once: true, rel: d}, nil
	}
	return spec{}, errors.New(grammarHelp)
}

// dayWords maps the day-set words "every <word> at ..." accepts. Both numbers
// are listed, since a model writing "every weekdays at 09:00" means the same
// thing and should not be corrected. An absent word reads as kindDaily, which
// is what lets the lookup double as the match test.
var dayWords = map[string]dayKind{
	"weekday": kindWeekdays, "weekdays": kindWeekdays,
	"weekend": kindWeekends, "weekends": kindWeekends,
}

// parts is an expression being taken apart, kept as two aligned slices so
// keywords match without regard to case while a timezone name keeps its own.
//
// A zone name is not ours to fold: time.LoadLocation resolves it against the
// host's tzdata, which on a case-sensitive filesystem refuses "asia/kolkata"
// outright and on a case-insensitive one resolves it under that name -- a name
// no tzdata carries, and the one that would then be stored in the expression
// and handed to glibc as TZ. Passing the token through as written is the only
// answer that means the same thing on both.
type parts struct {
	f    []string // lowercased, for matching
	orig []string // exactly as written
}

// fields splits an expression into its tokens.
func fields(expr string) *parts {
	orig := strings.Fields(strings.TrimSpace(expr))
	p := &parts{f: make([]string, len(orig)), orig: orig}
	for i, s := range orig {
		p.f[i] = strings.ToLower(s)
	}
	return p
}

// cut removes n tokens from index i. The three-index slice forces a copy rather
// than writing back over the tail both slices still share.
func (p *parts) cut(i, n int) {
	p.f = append(p.f[:i:i], p.f[i+n:]...)
	p.orig = append(p.orig[:i:i], p.orig[i+n:]...)
}

// find reports where a keyword is, or -1.
func (p *parts) find(kw string) int {
	for i, s := range p.f {
		if s == kw {
			return i
		}
	}
	return -1
}

// takeZone strips an "in <IANA name>" clause.
//
// Only a name carrying a region ("Asia/Kolkata") or UTC counts, which is what
// keeps "once in 2h" intact: "in" introduces both clauses, and what follows it
// is what tells them apart. A bare abbreviation is refused on purpose -- IST is
// India, Ireland and Israel, three different offsets, and time.LoadLocation
// resolves none of them.
func (p *parts) takeZone() (*time.Location, bool, error) {
	for i := 0; i+1 < len(p.f); i++ {
		name := p.orig[i+1]
		if p.f[i] != "in" || (!strings.Contains(name, "/") && p.f[i+1] != "utc") {
			continue
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			return nil, false, fmt.Errorf("%q is not a timezone such as Asia/Kolkata", name)
		}
		p.cut(i, 2)
		return loc, true, nil
	}
	return nil, false, nil
}

// takeUntil strips an "until <date>" clause.
//
// The bound is the end of the named day, not its midnight: someone who says
// "until the 31st" means through the 31st, and the other reading silently drops
// a day's worth of runs.
func (p *parts) takeUntil(loc *time.Location) (time.Time, bool, error) {
	i := p.find("until")
	if i < 0 {
		return time.Time{}, false, nil
	}
	if i+1 >= len(p.f) {
		return time.Time{}, false, errors.New(`"until" needs a date, such as until 2026-12-31`)
	}
	d, err := parseDate(p.f[i+1])
	if err != nil {
		return time.Time{}, false, err
	}
	p.cut(i, 2)
	end := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc).Add(-time.Nanosecond)
	return end, true, nil
}

// takeWindow strips a "between <time> and <time>" clause.
func (p *parts) takeWindow() (clock, clock, bool, error) {
	i := p.find("between")
	if i < 0 {
		return clock{}, clock{}, false, nil
	}
	if i+3 >= len(p.f) || p.f[i+2] != "and" {
		return clock{}, clock{}, false,
			errors.New(`"between" needs a range, such as between 09:00 and 17:00`)
	}
	start, err := parseClock(p.f[i+1])
	if err != nil {
		return clock{}, clock{}, false, err
	}
	end, err := parseClock(p.f[i+3])
	if err != nil {
		return clock{}, clock{}, false, err
	}
	if start == end {
		return clock{}, clock{}, false,
			errors.New("a between window has to start and end at different times")
	}
	p.cut(i, 4)
	return start, end, true, nil
}

// spread turns an interval held inside a window into the times of day it fires:
// "every 2h between 09:00 and 17:00" is 09:00 11:00 13:00 15:00 17:00.
//
// Anchored to the start of the window rather than to when the schedule happened
// to be created, so the same expression always means the same firings. A window
// that wraps past midnight ("between 22:00 and 06:00") falls out as an ordinary
// list of times, because it repeats every day exactly like one.
func spread(start, end clock, every time.Duration) []clock {
	step := max(int(every/time.Minute), 1)
	span := end.mins() - start.mins()
	if span < 0 {
		span += 24 * 60 // the window runs past midnight
	}
	var out []clock
	for m := 0; m <= span; m += step {
		at := (start.mins() + m) % (24 * 60)
		out = append(out, clock{at / 60, at % 60})
	}
	return tidyClocks(out)
}

// parseTimes reads the times of day at the tail of a clock form, which may be a
// list: "09:00 and 17:00".
//
// Everything left has to be a time, so a token that is not one is an error
// rather than something quietly dropped -- silently ignoring the tail of
// "daily at 09:00 and lunchtime" would book half of what was asked for.
func parseTimes(f []string) ([]clock, error) {
	var out []clock
	for _, tok := range f {
		if tok == "and" {
			continue
		}
		c, err := parseClock(strings.TrimSuffix(tok, ","))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("a time of day is missing, such as 09:00")
	}
	return tidyClocks(out), nil
}

// tidyClocks sorts times and drops repeats, so "daily at 09:00 and 09:00" is one
// firing rather than two a sweep apart.
//
// Sorts a copy. SortFunc and Compact both write through their argument, and a
// caller that still holds the times the person typed must not find them
// reordered and truncated under it.
func tidyClocks(in []clock) []clock {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b clock) int { return a.mins() - b.mins() })
	return slices.Compact(out)
}

// known reports whether m has an entry for key, without relying on the zero
// value being unreachable: dayKind's zero is kindDaily, a real kind, so a plain
// lookup on a miss reads as a match.
func known[K comparable, V any](m map[K]V, key K) bool {
	_, ok := m[key]
	return ok
}

// parseMonthDay reads "1st", "15" or "last day", returning 0 for the last day
// along with whatever follows it.
func parseMonthDay(f []string) (int, []string, error) {
	if len(f) >= 2 && f[0] == "last" && f[1] == "day" {
		return 0, f[2:], nil
	}
	if len(f) == 0 {
		return 0, nil, errors.New(`a monthly schedule needs a day, such as "the 1st" or "the last day"`)
	}
	n, err := strconv.Atoi(strings.TrimRight(f[0], "stndrh"))
	if err != nil || n < 1 || n > 31 {
		return 0, nil, fmt.Errorf("%q is not a day of the month such as 1st or 15", f[0])
	}
	return n, f[1:], nil
}

// dayUnits matches the day and week units time.ParseDuration does not know: it
// stops at hours, so "every 2d" fails and a model has to work out that it means
// 48h. Rewritten rather than rejected, because that arithmetic is exactly the
// kind this grammar exists to keep away from the model.
var dayUnits = regexp.MustCompile(`(\d+)([dw])`)

// expandDuration rewrites 2d and 1w into the hours Go can parse.
func expandDuration(s string) string {
	return dayUnits.ReplaceAllStringFunc(s, func(m string) string {
		g := dayUnits.FindStringSubmatch(m)
		n, _ := strconv.Atoi(g[1])
		if g[2] == "w" {
			n *= 7
		}
		return strconv.Itoa(n*24) + "h"
	})
}

// parseDate reads an ISO calendar date.
func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date such as 2026-09-02", s)
	}
	return t, nil
}

// parseClock reads a time of day.
//
// The am/pm layouts are leniency, not grammar: a model writes "9am" often
// enough, and rejecting it costs a turn to learn nothing. 24-hour stays the form
// the tool asks for, since it cannot be read two ways.
func parseClock(s string) (clock, error) {
	for _, layout := range []string{"15:04", "3:04pm", "3pm"} {
		if t, err := time.Parse(layout, s); err == nil {
			return clock{t.Hour(), t.Minute()}, nil
		}
	}
	return clock{}, fmt.Errorf("%q is not a time of day such as 09:00", s)
}

// daySearchLimit bounds the day walk in next. A year is well past every form
// here -- monthly needs at most 31 days -- so reaching it means the spec matches
// no day at all, and saying "no more runs" beats looping.
const daySearchLimit = 366

// next is the first firing strictly after the given time, or the zero time when
// the expression has no firing left -- which a one-off reaches after its single
// run, and any form reaches once it is past its "until".
func (sp spec) next(after time.Time) time.Time {
	at := sp.nextRaw(after)
	if at.IsZero() || (!sp.until.IsZero() && at.After(sp.until)) {
		return time.Time{}
	}
	return at
}

// nextRaw is next without the end date applied.
//
// The clock forms are built in the schedule's own location rather than by adding
// 24h, so a day that is 23 or 25 hours long over a DST change still fires at the
// stated wall-clock time. Each candidate day is built from the same year and
// month with the day number offset, which normalises across month and year ends
// without accumulating the drift that repeated adding would.
func (sp spec) nextRaw(after time.Time) time.Time {
	switch {
	case sp.rel > 0:
		return after.Add(sp.rel)
	case sp.once:
		if sp.at.After(after) {
			return sp.at
		}
		return time.Time{} // one occurrence, and it is behind us
	case sp.every > 0:
		return after.Add(sp.every)
	}
	t := after.In(sp.loc)
	y, mo, d := t.Date()
	for i := 0; i < daySearchLimit; i++ {
		day := time.Date(y, mo, d+i, 0, 0, 0, 0, sp.loc)
		if !sp.matchesDay(day) {
			continue
		}
		for _, c := range sp.times {
			at := time.Date(day.Year(), day.Month(), day.Day(), c.hour, c.minute, 0, 0, sp.loc)
			if at.After(after) {
				return at
			}
		}
	}
	return time.Time{}
}

// matchesDay reports whether a clock form fires on the given day at all.
func (sp spec) matchesDay(d time.Time) bool {
	switch sp.kind {
	case kindWeekly:
		return d.Weekday() == sp.day
	case kindWeekdays:
		return d.Weekday() != time.Saturday && d.Weekday() != time.Sunday
	case kindWeekends:
		return d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
	case kindMonthly:
		return d.Day() == sp.monthDay(d)
	}
	return true // kindDaily
}

// monthDay is which day of the month a monthly schedule lands on, clamped to the
// month's own length.
//
// "monthly on the 31st" means month-end to the person who wrote it -- rent, an
// invoice, a report. Skipping the seven months that have no 31st would drop most
// of the year, and letting the date overflow would silently fire on the 1st of
// the month after.
func (sp spec) monthDay(d time.Time) int {
	last := time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, sp.loc).Day()
	if sp.dom == 0 || sp.dom > last {
		return last
	}
	return sp.dom
}

// planSchedule works out what a new schedule built from an expression looks like:
// the expression to store, and when it first runs.
//
// It returns the expression because "once in 2h" only means anything at the
// moment it is written, and Expr is re-read on every sweep -- stored as typed it
// would mean two hours from each sweep and never come due. It is resolved to the
// instant it names, pinned to the zone that was current, so the stored form says
// the same thing forever and the person sees a real date in the app.
//
// The zero NextRunAt is the other reason this is one call rather than parse then
// next at each creation site: an expression can be perfectly well-formed and
// still have no run to book -- a date that has gone by, an "until" already past
// -- and a caller that only checked the parse would store a schedule that reads
// as due on every sweep for the rest of the machine's life.
func planSchedule(expr string, loc *time.Location, now time.Time) (spec, string, time.Time, error) {
	sp, err := parseSchedule(expr, loc)
	if err != nil {
		return spec{}, "", time.Time{}, err
	}
	if sp.rel > 0 {
		expr = absoluteForm(now.Add(sp.rel), loc)
		if sp, err = parseSchedule(expr, loc); err != nil {
			return spec{}, "", time.Time{}, err
		}
	}
	at := sp.next(now)
	if at.IsZero() {
		return spec{}, "", time.Time{}, noRunLeft(sp, loc)
	}
	return sp, expr, at, nil
}

// noRunLeft says why an expression that parsed has nothing to book.
func noRunLeft(sp spec, loc *time.Location) error {
	if sp.once {
		return fmt.Errorf("%s has already passed", sp.at.In(loc).Format("2006-01-02 15:04"))
	}
	return fmt.Errorf("there is no run before it ends on %s", sp.until.In(loc).Format("2006-01-02"))
}

// absoluteForm writes the one-off expression naming an instant.
//
// Rounded up to the minute because the grammar has no finer unit, and up rather
// than down so the run never lands before the moment that was asked for. The
// zone is named outright when it can be: a relative one-off is a fixed instant,
// and it should not slide because the person got on a plane. A location with no
// IANA name -- all a client sending only an offset can give us -- is left off,
// and that schedule follows the person as the clock forms do.
func absoluteForm(at time.Time, loc *time.Location) string {
	if up := at.Truncate(time.Minute); up.Before(at) {
		at = up.Add(time.Minute)
	}
	in := at.In(loc)
	expr := "once on " + in.Format("2006-01-02") + " at " + in.Format("15:04")
	if name := loc.String(); strings.Contains(name, "/") || name == "UTC" {
		expr += " in " + name
	}
	return expr
}

// zonePath is where the client's timezone is remembered. One file for the
// machine, like about-the-person.md: everyone here works for the same person.
func zonePath(stateDir string) string { return filepath.Join(stateDir, "timezone") }

// RememberZone records where the person says they are.
//
// An IANA name and nothing else. The app asks for a country at onboarding and
// resolves it to a zone before it gets here, so there is no stamp to fall back
// on and no reason to want one: an offset is only true for the instant it was
// sampled, so a schedule stored from +05:30 in July keeps firing at July's
// offset after a daylight-saving change, while a named zone stays correct.
//
// An unresolvable name is ignored rather than stored. Failure is silent -- not
// knowing the zone costs UTC, and it must never fail the person's onboarding.
// Reports whether it stored anything, which is what tells the caller the zone
// actually moved. Unexported: outside this file the only way to set a zone is
// the Supervisor method below, which is what carries the adoption with it.
func rememberZone(stateDir, tz string) bool {
	if tz == readZoneFile(stateDir) || validZone(tz) != nil {
		return false
	}
	return os.WriteFile(zonePath(stateDir), []byte(tz), 0o640) == nil
}

// validZone reports whether this is a name the machine can adopt, and is the
// one place that rule lives -- the HTTP handler refuses what it refuses, so a
// zone cannot be accepted with a 204 and then silently dropped here.
//
// An IANA name and nothing else. "Local" resolves but means whatever zone the
// process happens to be in, which is the ambient answer this whole design
// exists to remove; "" resolves to UTC, which is the absence of an answer
// rather than one.
func validZone(tz string) error {
	if tz == "" || tz == "Local" {
		return fmt.Errorf("%q is not a timezone such as Asia/Kolkata", tz)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("%q is not a timezone such as Asia/Kolkata", tz)
	}
	return nil
}

// RememberZone records the zone and, when it has actually changed, adopts it:
// the daemon's own TZ moves, and the clock schedules move with it.
//
// Moving the schedules is the point. A stored NextRunAt is an absolute instant,
// so a person who books "daily at 09:00" in London and then changes their
// country to the United States would get one firing at the old 9am -- 01:00
// where they now are -- before the schedule settled onto the new zone.
// Re-saving the same profile writes nothing and moves nothing.
func (s *Supervisor) RememberZone(tz string) bool {
	if !rememberZone(s.stateDir, tz) {
		return false
	}
	exportZone(tz)
	s.rezone(time.Now())
	return true
}

// AdoptZone puts the whole guest on the person's clock, and is called once at
// startup before anything else runs.
//
// This is what makes the zone a property of the machine rather than something
// threaded through every request. The VM belongs to one person -- its id is
// derived from their account -- so "the local time here" is a coherent idea in
// a way it never is on a shared host, and once the guest is on their clock,
// every reader is right for free: `date` in a bash tool, a file mtime an agent
// lists, the date a task folder is named after.
//
// Called from main before the supervisor exists, so the assignment to
// time.Local cannot race a goroutine reading it. A later change comes in
// through RememberZone, which moves TZ for the subprocesses that care and
// leaves time.Local alone until the next restart: assigning it under a running
// daemon would race the sweep and every live turn, and restarting to avoid that
// would drop the calls onboarding makes right after saving the profile.
//
// So nothing in the daemon may depend on time.Local for its answer: everything
// that needs the person's clock takes it from loadZone, which is correct the
// moment onboarding writes the file.
func AdoptZone(stateDir string) {
	zone := readZoneFile(stateDir)
	if zone == "" {
		return
	}
	// loadZone rather than LoadLocation, so a machine still holding the old
	// offset form moves with everything else that reads it. Resolving the name
	// here on its own would leave the daemon on UTC while its own schedules ran
	// on the person's offset -- and that cohort is the only reason the fallback
	// in loadZone exists.
	time.Local = loadZone(stateDir)
	// TZ takes a name only: glibc cannot read an offset stamp, and handing it one
	// would put every subprocess on something neither side meant.
	if validZone(zone) == nil {
		exportZone(zone)
	}
}

// exportZone puts the zone in the environment, where the processes an agent
// starts will find it.
//
// A bash tool inherits this daemon's environment at exec time, and glibc reads
// TZ before it reads /etc/localtime -- which matters because agentd runs as
// `agent` and cannot write /etc/localtime anyway. So this one setenv is what
// makes `date` inside a turn print the person's time instead of UTC.
func exportZone(zone string) {
	os.Setenv("TZ", zone)
}

// personNow is the current time on the person's own clock.
//
// The one accessor for it, so a person-facing date is never accidentally the
// guest's. Reads the stored zone rather than time.Local, which AdoptZone sets
// at boot and which is therefore still UTC on a machine onboarded since.
func personNow(stateDir string) time.Time {
	return time.Now().In(loadZone(stateDir))
}

// readZoneFile returns the stored zone name, or "" when there is not one yet.
// Trimmed here so no caller has to defend against the trailing newline a hand
// edit of the file would leave.
func readZoneFile(stateDir string) string {
	buf, err := os.ReadFile(zonePath(stateDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf))
}

// rezone recomputes the next firing of every enabled clock schedule.
//
// "every 30m" is left alone: an interval is not anchored to a wall clock, so
// moving it would only push the next run further out. A one-off does move, for
// the same reason a daily does: "once on 2026-09-02 at 11:00" names a wall-clock
// instant, and the person who wrote it means 11:00 where they are. A schedule
// that named its own zone is left alone too -- that is what naming it was for,
// and it is how a run tied to something outside the person, a sale opening at
// 11:00 in Kolkata, stays put when they fly somewhere else. A schedule whose
// expression no longer parses is left for the sweep, which disables it and says
// why rather than silently skipping it here.
func (s *Supervisor) rezone(now time.Time) {
	loc := loadZone(s.stateDir)
	for _, sc := range s.schedules.List() {
		if !sc.Enabled {
			continue
		}
		sp, err := parseSchedule(sc.Expr, loc)
		if err != nil || sp.every > 0 || sp.pinned {
			continue
		}
		// A schedule with no run left has no next: leave the stored instant alone
		// so the sweep can still judge it against oneShotGrace.
		at := sp.next(now)
		if at.IsZero() {
			continue
		}
		s.schedules.Update(sc.ID, func(x *Schedule) { x.NextRunAt = at })
	}
}

// loadZone resolves the remembered timezone, falling back to UTC.
func loadZone(stateDir string) *time.Location {
	v := readZoneFile(stateDir)
	if loc, err := time.LoadLocation(v); err == nil {
		return loc
	}
	// A machine onboarded before the zone became a name may hold an RFC3339
	// stamp, which is what the old code stored when a client sent no IANA name.
	// Read it rather than dropping to UTC: that machine's schedules are anchored
	// to this offset, and falling back would silently re-anchor every one of them
	// at the next boot with no way left to say where the person is.
	//
	// Nothing writes this form any more -- rememberZone refuses it. Those
	// machines convert themselves: the stamp comes back on GET /person, the app
	// cannot name a country for it and shows "Not set", and one tap stores a real
	// name. This branch is deletable once that cohort has been through the app,
	// and not before -- it is a read path only, so leaving it costs one failed
	// LoadLocation on machines that no longer need it.
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
// The LAST occurrence is the opposite case and is handled the opposite way --
// a one-off, or the final run of a schedule that has reached its "until". There
// is nothing behind it to fall back on, so spending it on a busy agent would
// mean the thing the person asked for never happens at all. It is left due and
// retried on each sweep instead, bounded by oneShotGrace: a machine that is busy
// all afternoon should say the run was missed rather than deliver it at midnight
// or hold it due forever.
//
// next returning the zero time is what says "this was the last one", so the two
// cases need no separate flag and cannot drift apart.
func (s *Supervisor) fire(sc Schedule, now time.Time) {
	sp, err := parseSchedule(sc.Expr, loadZone(s.stateDir))
	if err != nil {
		s.disable(sc, "its schedule no longer parses: "+err.Error())
		return
	}
	at := sp.next(now)
	last := at.IsZero()
	switch {
	case !last:
		s.schedules.Update(sc.ID, func(x *Schedule) { x.NextRunAt = at })
	case now.Sub(sc.NextRunAt) > oneShotGrace:
		s.disable(sc, "its last run came and went with nothing free to take it")
		return
	}
	if _, ok := s.roster.Get(sc.Agent); !ok {
		s.disable(sc, "the agent it belonged to is gone")
		return
	}
	if !s.idle(sc.Agent) {
		return // busy with this or with the person's own work; catch it next time
	}
	_, err = s.sendTo(sc.Agent, func(a *Agent) error { return a.SendScheduled(sc.Name, sc.Task) })
	if err != nil {
		return // no capacity right now, which the next occurrence may well have
	}
	s.schedules.Update(sc.ID, func(x *Schedule) {
		x.LastFired, x.Fires = now, x.Fires+1
		// A schedule with nothing after this run is spent by it. Turned off
		// rather than deleted, so the person opening the app sees that the
		// thing they asked for did happen instead of finding an empty list.
		x.Enabled = !last
	})
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
		// A schedule with no run left -- a one-off, or one past its "until" -- is
		// left exactly where it is. There is nothing to skip forward to, and
		// moving it would write a zero NextRunAt that reads as due on every sweep
		// from here on. Whether the machine came back soon enough for that last
		// run to still be worth making is the sweep's call, which is the one
		// place that can turn it off and say why.
		at := sp.next(now)
		if at.IsZero() {
			continue
		}
		s.schedules.Update(sc.ID, func(x *Schedule) { x.NextRunAt = at })
	}
}
