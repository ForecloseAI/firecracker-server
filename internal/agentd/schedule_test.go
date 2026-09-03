package agentd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// mustZone resolves a location or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The grammar is the whole surface a model has to hit, so every accepted form
// has to parse and every rejected one has to say what would work instead.
func TestParseSchedule(t *testing.T) {
	for _, tc := range []struct {
		expr    string
		wantErr string
	}{
		{expr: "every 30m"},
		{expr: "every 6h"},
		{expr: "daily at 09:00"},
		{expr: "weekly on mon at 09:00"},
		{expr: "once on 2026-09-02 at 11:00"},
		{expr: "once in 2h"},
		{expr: "once in 20m"}, // no 15m floor: a one-off spends exactly one turn
		{expr: "every 2d"},    // ParseDuration stops at hours; these are rewritten
		{expr: "every 1w"},
		{expr: "every weekday at 09:00"},
		{expr: "every weekdays at 09:00"}, // either number reads the same
		{expr: "every weekend at 10:00"},
		{expr: "monthly on the 1st at 09:00"},
		{expr: "monthly on the 31st at 09:00"},
		{expr: "monthly on the last day at 18:00"},
		{expr: "monthly on 15 at 09:00"}, // "the" is optional and so is the ordinal
		{expr: "daily at 09:00 and 17:00"},
		{expr: "daily at 09:00 and 13:00 and 17:00"},
		{expr: "weekly on mon at 09:00 and 17:00"},
		{expr: "daily at 9am"},    // leniency: models write this
		{expr: "daily at 9:30pm"}, //
		{expr: "every 2h between 09:00 and 17:00"},
		{expr: "every 2h between 22:00 and 06:00"}, // a window may wrap midnight
		{expr: "daily at 09:00 until 2026-12-31"},
		{expr: "daily at 09:00 in Asia/Kolkata"},
		{expr: "every 30m in UTC"},
		{expr: "every 2h between 09:00 and 17:00 until 2026-12-31 in Asia/Kolkata"},
		{expr: "every weekday at 09:00 and 17:00 until 2026-12-31 in Europe/London"},
		{expr: "EVERY 30M"},                          // case is not the model's problem
		{expr: "  daily at 23:59   "},                // nor is stray spacing
		{expr: "  ONCE ON 2026-09-02 AT 11:00     "}, // and the one-off is no different
		{expr: "DAILY AT 09:00 IN Asia/Kolkata"},     // but a zone name keeps its own case
		{expr: "every 5m", wantErr: "tightest"},
		{expr: "every 14m59s", wantErr: "tightest"},
		{expr: "every banana", wantErr: "duration"},
		{expr: "daily at 25:00", wantErr: "time of day"},
		{expr: "weekly on funday at 09:00", wantErr: "day such as"},
		{expr: "once on 02-09-2026 at 11:00", wantErr: "date such as"}, // not ISO
		{expr: "once on 2026-13-02 at 11:00", wantErr: "date such as"}, // no such month
		{expr: "once on 2026-09-02 at 25:00", wantErr: "time of day"},
		{expr: "once at 2026-09-02 11:00", wantErr: "once on"}, // near miss, told the form
		{expr: "0 9 * * *", wantErr: "every 30m"},              // cron gets told the real grammar
		{expr: "", wantErr: "every 30m"},
		// The clauses each have to refuse the base forms they make no sense on,
		// rather than being accepted and quietly doing nothing.
		{expr: "once on 2026-09-02 at 11:00 until 2026-12-31", wantErr: "no meaning on a one-off"},
		{expr: "once in 2h until 2026-12-31", wantErr: "no meaning on a one-off"},
		{expr: "daily at 09:00 between 09:00 and 17:00", wantErr: `only goes with "every`},
		{expr: "every weekday at 09:00 between 09:00 and 17:00", wantErr: `only goes with "every`},
		{expr: "once in 2h in Asia/Kolkata", wantErr: "measured from now"},
		{expr: "every 2h between 09:00 and 09:00", wantErr: "different times"},
		{expr: "every 2h between 09:00", wantErr: "needs a range"},
		{expr: "every 2h between 09:00 to 17:00", wantErr: "needs a range"},
		{expr: "daily at 09:00 until", wantErr: "needs a date"},
		{expr: "daily at 09:00 until soon", wantErr: "date such as"},
		{expr: "daily at 09:00 in Mars/Olympus", wantErr: "not a timezone"},
		{expr: "daily at 09:00 in asia/kolkata", wantErr: "not a timezone"}, // case matters
		{expr: "monthly on the 0th at 09:00", wantErr: "day of the month"},
		{expr: "monthly on the 32nd at 09:00", wantErr: "day of the month"},
		{expr: "monthly on the 1st", wantErr: "needs a time"},
		{expr: "daily at 09:00 and lunchtime", wantErr: "time of day"}, // no silent drop
		{expr: "once on 2026-09-02 at 11:00 and 15:00", wantErr: "once on"},
	} {
		_, err := parseSchedule(tc.expr, time.UTC)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: %v", tc.expr, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%q was accepted; want an error mentioning %q", tc.expr, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%q: error %q does not mention %q", tc.expr, err, tc.wantErr)
		}
	}
}

// An expression the grammar does not know is the moment a model finds out what
// the grammar does know, and the one-off form has to be in that list. It is the
// form nothing else substitutes for: told only about the repeating three, a
// model asked for a single dated event books a daily job with a date check in
// its task text, which is the bug this form exists to remove.
func TestTheGrammarErrorNamesEveryForm(t *testing.T) {
	_, err := parseSchedule("0 9 * * *", time.UTC)
	if err == nil {
		t.Fatal("cron was accepted")
	}
	for _, want := range []string{"every 30m", "daily at", "weekly on", "once on"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the grammar error does not offer %q: %s", want, err)
		}
	}
}

// A firing that is not strictly in the future fires again immediately, and the
// sweep would then loop on it every tick.
func TestNextIsAlwaysInTheFuture(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) // exactly on a daily boundary
	for _, expr := range []string{"every 30m", "daily at 09:00", "weekly on sun at 09:00",
		"once on 2026-09-02 at 11:00"} {
		sp, err := parseSchedule(expr, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.next(now); !got.After(now) {
			t.Errorf("%q: next(%v) = %v, which is not in the future", expr, now, got)
		}
	}
}

// "daily at 09:00" means 9am where the person is. Built in their location rather
// than by adding 24h, so the 23-hour day over a spring-forward still lands on 9.
func TestDailyHoldsItsWallClockAcrossDST(t *testing.T) {
	london := mustZone(t, "Europe/London")
	sp, err := parseSchedule("daily at 09:00", london)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29 is the UK spring-forward. Start the evening before.
	before := time.Date(2026, 3, 28, 20, 0, 0, 0, london)
	got := sp.next(before).In(london)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("next = %v, want 09:00 local", got)
	}
	if got.Day() != 29 {
		t.Errorf("next = %v, want the 29th", got)
	}
}

// Weekly has to reach the right day, not just the right hour.
func TestWeeklyLandsOnItsDay(t *testing.T) {
	sp, err := parseSchedule("weekly on fri at 17:30", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	got := sp.next(time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)) // a Monday
	if got.Weekday() != time.Friday || got.Hour() != 17 || got.Minute() != 30 {
		t.Errorf("next = %v, want the coming Friday at 17:30", got)
	}
}

// The one-off form names an instant rather than a rhythm, so it has to land on
// exactly that instant in the person's own zone. This is the case that was
// asked for and could not be said: a sale opening at 11:00 IST on 2 September.
func TestOnceLandsOnItsNamedInstant(t *testing.T) {
	kolkata := mustZone(t, "Asia/Kolkata")
	sp, err := parseSchedule("once on 2026-09-02 at 11:00", kolkata)
	if err != nil {
		t.Fatal(err)
	}

	got := sp.next(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)).UTC()

	want := time.Date(2026, 9, 2, 5, 30, 0, 0, time.UTC) // 11:00 +05:30
	if !got.Equal(want) {
		t.Errorf("next = %v, want %v", got, want)
	}
}

// A one-off has exactly one occurrence. next has to say so rather than invent a
// second one, because everything downstream reads a stored NextRunAt as a
// promise that there is still a run to come.
func TestOnceHasNoRunAfterItsOwn(t *testing.T) {
	sp, err := parseSchedule("once on 2026-09-02 at 11:00", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	at := sp.next(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC))

	if got := sp.next(at); !got.IsZero() {
		t.Errorf("next after the single occurrence = %v, want the zero time", got)
	}
}

// A date that has gone by parses perfectly well and still has nothing to book.
// Caught at creation, because a schedule stored with a zero NextRunAt reads as
// due on every sweep for the rest of the machine's life.
func TestAPastOneOffIsRefusedWhenItIsCreated(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

	if _, err := parseSchedule("once on 2026-09-02 at 11:00", time.UTC); err != nil {
		t.Fatalf("the expression itself is well-formed: %v", err)
	}
	_, _, at, err := planSchedule("once on 2026-09-02 at 11:00", time.UTC, now)
	if err == nil {
		t.Fatalf("a one-off in the past was booked for %v", at)
	}
	if !strings.Contains(err.Error(), "already passed") {
		t.Errorf("error = %q, want it to say the time has passed", err)
	}
	if _, _, at, err := planSchedule("daily at 09:00", time.UTC, now); err != nil || !at.After(now) {
		t.Errorf("a repeating form was caught by the past-date check: %v %v", at, err)
	}
}

// wantRuns compares an expression's first len(want) firings, as readable
// stamps, against what they should be. Comparing a whole sequence rather than
// one next() catches the errors that matter here: a day-of-week filter that
// lands right once and then drifts, a monthly clamp that works in April and not
// in February, a time list that never reaches its second entry.
func wantRuns(t *testing.T, expr string, loc *time.Location, from time.Time, want ...string) {
	t.Helper()
	sp, err := parseSchedule(expr, loc)
	if err != nil {
		t.Fatalf("%q: %v", expr, err)
	}
	got := make([]string, 0, len(want))
	at := from
	for range want {
		if at = sp.next(at); at.IsZero() {
			got = append(got, "no more runs")
			break
		}
		// Reported in the schedule's own zone, not the person's: a pinned
		// expression means its own wall clock, and showing the person's would
		// make a correct firing look wrong.
		got = append(got, at.In(sp.loc).Format("Mon 2006-01-02 15:04"))
	}
	if !slices.Equal(got, want) {
		t.Errorf("%q\n got %v\nwant %v", expr, got, want)
	}
}

// A morning brief on workdays is the reason this form exists, and the weekend is
// the whole of its behaviour: without the skip it is just "daily".
func TestWeekdaysSkipTheWeekend(t *testing.T) {
	friday := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC) // a Friday, after 09:00
	wantRuns(t, "every weekday at 09:00", time.UTC, friday,
		"Mon 2026-03-09 09:00",
		"Tue 2026-03-10 09:00",
		"Wed 2026-03-11 09:00",
		"Thu 2026-03-12 09:00",
		"Fri 2026-03-13 09:00",
		"Mon 2026-03-16 09:00", // and over the weekend again
	)
}

// The mirror image, and worth its own test: an off-by-one in the day filter that
// still skips two days a week would pass a weekday test on its own.
func TestWeekendsPickSaturdayAndSunday(t *testing.T) {
	friday := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	wantRuns(t, "every weekend at 10:00", time.UTC, friday,
		"Sat 2026-03-07 10:00",
		"Sun 2026-03-08 10:00",
		"Sat 2026-03-14 10:00",
	)
}

// "The 31st" means month-end to whoever wrote it -- rent, an invoice, a report.
// Skipping the seven months without a 31st would drop most of the year, and
// letting the date overflow would fire on the 1st of the month after, which is
// the same silent wrong-day this whole grammar exists to avoid.
func TestMonthlyClampsToShortMonths(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantRuns(t, "monthly on the 31st at 09:00", time.UTC, start,
		"Sat 2026-01-31 09:00",
		"Sat 2026-02-28 09:00", // clamped, not skipped and not 2026-03-03
		"Tue 2026-03-31 09:00",
		"Thu 2026-04-30 09:00", // clamped again
	)
}

// The last day is its own phrasing because it is what people mean, and it has to
// find February 29th in a leap year rather than a hardcoded 28.
func TestMonthlyOnTheLastDayFindsLeapFebruary(t *testing.T) {
	start := time.Date(2028, 1, 15, 0, 0, 0, 0, time.UTC)
	wantRuns(t, "monthly on the last day at 18:00", time.UTC, start,
		"Mon 2028-01-31 18:00",
		"Tue 2028-02-29 18:00", // 2028 is a leap year
		"Fri 2028-03-31 18:00",
	)
}

// An ordinary day of the month must not be dragged to month-end by the clamp.
func TestMonthlyOnAnEarlyDayIsNotClamped(t *testing.T) {
	start := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	wantRuns(t, "monthly on the 1st at 09:00", time.UTC, start,
		"Sun 2026-02-01 09:00",
		"Sun 2026-03-01 09:00",
	)
}

// A time list has to reach every entry and then roll to the next day, in order.
// Sorted at parse time, so the order it was written in does not matter.
func TestSeveralTimesADayRunInOrder(t *testing.T) {
	start := time.Date(2026, 3, 6, 8, 0, 0, 0, time.UTC)
	want := []string{
		"Fri 2026-03-06 09:00",
		"Fri 2026-03-06 13:00",
		"Fri 2026-03-06 17:00",
		"Sat 2026-03-07 09:00",
	}
	wantRuns(t, "daily at 09:00 and 13:00 and 17:00", time.UTC, start, want...)
	wantRuns(t, "daily at 17:00 and 09:00 and 13:00", time.UTC, start, want...)
	// A repeat is one firing, not two a sweep apart.
	wantRuns(t, "daily at 09:00 and 09:00 and 17:00", time.UTC, start,
		"Fri 2026-03-06 09:00", "Fri 2026-03-06 17:00", "Sat 2026-03-07 09:00")
}

// A list combines with a day filter rather than replacing it.
func TestSeveralTimesCombineWithADayFilter(t *testing.T) {
	friday := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	wantRuns(t, "every weekday at 09:00 and 17:00", time.UTC, friday,
		"Fri 2026-03-06 17:00",
		"Mon 2026-03-09 09:00", // not Saturday
		"Mon 2026-03-09 17:00",
	)
}

// The window is anchored to its own start, not to when the schedule was made, so
// the same expression always means the same firings. Then it stops for the night
// rather than carrying the interval on through 19:00 and 21:00.
func TestAWindowFiresOnItsOwnGridAndStopsForTheNight(t *testing.T) {
	start := time.Date(2026, 3, 6, 8, 30, 0, 0, time.UTC) // created mid-morning
	wantRuns(t, "every 2h between 09:00 and 17:00", time.UTC, start,
		"Fri 2026-03-06 09:00", // 09:00, not 10:30
		"Fri 2026-03-06 11:00",
		"Fri 2026-03-06 13:00",
		"Fri 2026-03-06 15:00",
		"Fri 2026-03-06 17:00",
		"Sat 2026-03-07 09:00", // not 19:00
	)
}

// An overnight window is a real thing to ask for and must not come out empty or
// inverted. It repeats daily like any other, so it falls out as a plain list.
func TestAWindowMayWrapPastMidnight(t *testing.T) {
	start := time.Date(2026, 3, 6, 20, 0, 0, 0, time.UTC)
	wantRuns(t, "every 2h between 22:00 and 06:00", time.UTC, start,
		"Fri 2026-03-06 22:00",
		"Sat 2026-03-07 00:00",
		"Sat 2026-03-07 02:00",
		"Sat 2026-03-07 04:00",
		"Sat 2026-03-07 06:00",
		"Sat 2026-03-07 22:00", // and nothing in the daytime
	)
}

// A window shorter than its own interval is not an error, just a schedule that
// fires once a day. Worth pinning: the obvious implementation returns nothing.
func TestAWindowShorterThanItsIntervalStillFires(t *testing.T) {
	start := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
	wantRuns(t, "every 6h between 09:00 and 10:00", time.UTC, start,
		"Fri 2026-03-06 09:00",
		"Sat 2026-03-07 09:00",
	)
}

// "until the 31st" means through the 31st. Ending at its midnight instead would
// silently drop that day's runs, which is the day people are usually thinking of.
func TestUntilIncludesItsOwnDayAndThenStops(t *testing.T) {
	start := time.Date(2026, 12, 29, 12, 0, 0, 0, time.UTC)
	wantRuns(t, "daily at 09:00 until 2026-12-31", time.UTC, start,
		"Wed 2026-12-30 09:00",
		"Thu 2026-12-31 09:00", // the day named still runs
		"no more runs",
	)
}

// The end date is read in the schedule's own zone, so it bounds the person's day
// rather than the guest's UTC one.
func TestUntilIsReadInTheSchedulesOwnZone(t *testing.T) {
	kolkata := mustZone(t, "Asia/Kolkata")
	start := time.Date(2026, 12, 30, 0, 0, 0, 0, kolkata)
	// 23:00 IST on the 31st is 17:30 UTC on the 31st -- inside the day either
	// way -- but 01:00 IST on the 31st is 19:30 UTC on the 30th, and a bound
	// built in UTC would cut the series a day early.
	wantRuns(t, "daily at 23:00 until 2026-12-31", kolkata, start,
		"Wed 2026-12-30 23:00",
		"Thu 2026-12-31 23:00",
		"no more runs",
	)
}

// until composes with the day filters rather than being checked only for daily.
func TestUntilBoundsAWeekdaySeries(t *testing.T) {
	friday := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	wantRuns(t, "every weekday at 09:00 until 2026-03-10", time.UTC, friday,
		"Mon 2026-03-09 09:00",
		"Tue 2026-03-10 09:00",
		"no more runs",
	)
}

// Every clause at once, since they are parsed by stripping one at a time and a
// bad cut would only show up when more than one is present.
func TestEveryClauseAtOnce(t *testing.T) {
	kolkata := mustZone(t, "Asia/Kolkata")
	start := time.Date(2026, 3, 6, 0, 0, 0, 0, kolkata) // a Friday
	wantRuns(t, "every 4h between 09:00 and 17:00 until 2026-03-07 in Asia/Kolkata",
		time.UTC, start, // the person is in UTC; the expression overrides that
		"Fri 2026-03-06 09:00",
		"Fri 2026-03-06 13:00",
		"Fri 2026-03-06 17:00",
		"Sat 2026-03-07 09:00",
		"Sat 2026-03-07 13:00",
		"Sat 2026-03-07 17:00",
		"no more runs",
	)
}

// A named zone in the expression wins over the person's, and the instant it
// produces is the one that zone means.
func TestANamedZoneOverridesThePersons(t *testing.T) {
	sp, err := parseSchedule("daily at 11:00 in Asia/Kolkata", mustZone(t, "Europe/London"))
	if err != nil {
		t.Fatal(err)
	}
	if !sp.pinned {
		t.Error("a schedule naming its own zone is not marked pinned; rezone would move it")
	}
	got := sp.next(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)).UTC()
	want := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC) // 11:00 +05:30
	if !got.Equal(want) {
		t.Errorf("next = %v, want %v", got, want)
	}
}

// A time list holds its wall clock across a DST change like a single time does:
// built in the location rather than by adding 24h to the previous firing.
func TestSeveralTimesHoldTheirWallClockAcrossDST(t *testing.T) {
	london := mustZone(t, "Europe/London")
	// 2026-03-29 is the UK spring-forward; start the evening before.
	before := time.Date(2026, 3, 28, 20, 0, 0, 0, london)
	wantRuns(t, "daily at 09:00 and 17:00", london, before,
		"Sun 2026-03-29 09:00", // still 09:00 local on a 23-hour day
		"Sun 2026-03-29 17:00",
		"Mon 2026-03-30 09:00",
	)
}

// Go's ParseDuration stops at hours, so these would be rejected outright and a
// model would have to convert them by hand -- the arithmetic this grammar exists
// to keep away from it.
func TestDayAndWeekDurations(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want time.Duration
	}{
		{"every 2d", 48 * time.Hour},
		{"every 1w", 7 * 24 * time.Hour},
		{"every 1d12h", 36 * time.Hour},
		{"every 24h", 24 * time.Hour},
	} {
		sp, err := parseSchedule(tc.expr, time.UTC)
		if err != nil {
			t.Errorf("%q: %v", tc.expr, err)
			continue
		}
		if sp.every != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, sp.every, tc.want)
		}
	}
}

// The am/pm forms are leniency and have to mean what they say -- 12am and 12pm
// are the pair that get written backwards.
func TestAmPmTimesAreReadCorrectly(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want clock
	}{
		{"9am", clock{9, 0}}, {"9:30pm", clock{21, 30}},
		{"12am", clock{0, 0}}, {"12pm", clock{12, 0}}, {"09:00", clock{9, 0}},
	} {
		got, err := parseClock(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseClock(%q) = %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
}

// The country picked at onboarding resolves to an IANA name, that name is what
// the machine keeps, and it is what the schedule math reads with no client
// involved.
func TestScheduleUsesTheStoredZone(t *testing.T) {
	dir := t.TempDir()
	rememberZone(dir, "Asia/Kolkata")
	if got := loadZone(dir).String(); got != "Asia/Kolkata" {
		t.Errorf("zone = %q, want Asia/Kolkata", got)
	}
	sp, err := parseSchedule("daily at 09:00", loadZone(dir))
	if err != nil {
		t.Fatal(err)
	}
	got := sp.next(time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)).UTC()
	if got.Hour() != 3 || got.Minute() != 30 {
		t.Errorf("09:00 in Asia/Kolkata = %v UTC, want 03:30", got)
	}
}

// Nothing sent, or nonsense sent, must not leave the zone in a broken state.
func TestZoneDefaultsToUTC(t *testing.T) {
	dir := t.TempDir()
	if got := loadZone(dir); got != time.UTC {
		t.Errorf("zone with no file = %v, want UTC", got)
	}
	rememberZone(dir, "Mars/Olympus")
	if got := loadZone(dir); got != time.UTC {
		t.Errorf("zone after nonsense = %v, want UTC", got)
	}
}

// An offset is not a zone. A client sending one used to have it stored verbatim,
// which pinned every "daily at 09:00" to the offset in force the day it was
// sampled and fired an hour out after the next daylight-saving change.
func TestRememberZoneRefusesAnOffset(t *testing.T) {
	dir := t.TempDir()
	rememberZone(dir, "Asia/Kolkata")
	rememberZone(dir, "2026-08-29T14:03:11+09:00")
	if got := loadZone(dir).String(); got != "Asia/Kolkata" {
		t.Errorf("zone = %q, want Asia/Kolkata: a stamp must not replace a name", got)
	}
}

// AdoptZone is what puts the guest itself on the person's clock: the daemon's
// own time.Local moves, and TZ is exported so a bash tool's `date` agrees.
func TestAdoptZoneMovesTheProcess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TZ", "")
	local := time.Local
	t.Cleanup(func() { time.Local = local })

	rememberZone(dir, "Asia/Kolkata")
	AdoptZone(dir)

	if got := time.Local.String(); got != "Asia/Kolkata" {
		t.Errorf("time.Local = %q, want Asia/Kolkata", got)
	}
	if got := os.Getenv("TZ"); got != "Asia/Kolkata" {
		t.Errorf("TZ = %q, want Asia/Kolkata: subprocesses read this, not time.Local", got)
	}
}

// A machine that has never been onboarded must boot, not panic or wander off
// the host's own zone.
func TestAdoptZoneWithNoZoneIsANoOp(t *testing.T) {
	local := time.Local
	t.Cleanup(func() { time.Local = local })
	AdoptZone(t.TempDir())
	if time.Local != local {
		t.Errorf("time.Local moved to %v with no zone stored", time.Local)
	}
}

// Ids must not be reused after a deletion: a client holding the old one would
// otherwise cancel a schedule it has never seen.
func TestScheduleIDsAreNotReused(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Add(Schedule{Name: "one"})
	second, _ := store.Add(Schedule{Name: "two"})
	store.Delete(second.ID)
	third, _ := store.Add(Schedule{Name: "three"})
	if third.ID == second.ID || third.ID == first.ID {
		t.Errorf("id %s was reused after a deletion", third.ID)
	}
}

// A cancellation that only happened in memory is worse than a refused one: the
// job comes back at the next restart while the person has been told it is gone.
func TestDeleteReportsAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.Add(Schedule{Name: "sweep", Agent: BossID, Expr: "every 30m"})
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the temporary file goes makes the write fail the way a
	// full or read-only disk would.
	if err := os.Mkdir(store.path+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.Delete(added.ID)

	if err == nil {
		t.Fatal("a delete that could not be written reported success")
	}
	if deleted {
		t.Error("delete reported the schedule gone despite the failure")
	}
	if got := store.List(); len(got) != 1 {
		t.Errorf("the store holds %d schedules, want the one that could not be deleted", len(got))
	}
}

// The store is the durable half of the feature; a schedule that does not survive
// a restart is one the person agreed to and never gets.
func TestSchedulesSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.Add(Schedule{Name: "sweep", Agent: BossID, Expr: "every 30m"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.List()
	if len(got) != 1 || got[0].ID != added.ID || got[0].Name != "sweep" {
		t.Fatalf("reload returned %+v, want the schedule that was added", got)
	}
	if !got[0].Enabled {
		t.Error("a reloaded schedule came back disabled")
	}
}

// A damaged file must stop the daemon rather than start empty, or the next write
// silently deletes everything the person agreed to.
func TestCorruptScheduleFileIsFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "schedules.json"), []byte("{not json"), 0o640)
	if _, err := LoadSchedules(dir); err == nil {
		t.Error("a corrupt schedules.json loaded cleanly; it must refuse")
	}
}

// The framing is what stops the model reading a timer as the person speaking,
// and what tells it that asking a question costs a night rather than a moment.
func TestScheduledFramingSaysNobodyMayBeWatching(t *testing.T) {
	got := frame(inbound{text: "check the deploy", schedule: "morning sweep"})
	for _, want := range []string{"morning sweep", "not the person", "at once"} {
		if !strings.Contains(got, want) {
			t.Errorf("scheduled framing does not mention %q: %s", want, got)
		}
	}
	if plain := frame(inbound{text: "check the deploy"}); plain != "check the deploy" {
		t.Errorf("an ordinary message was reframed: %q", plain)
	}
}

// The transcript must never show the person saying words they did not type.
func TestAScheduledFireIsNotLoggedAsTheUser(t *testing.T) {
	a := newTestAgent(t)
	if err := a.SendScheduled("morning sweep", "check the deploy"); err != nil {
		t.Fatal(err)
	}
	events, err := a.log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Type == "user" {
			t.Error("a scheduled fire was logged as a message from the person")
		}
		if e.Type == "scheduled" {
			found = true
			if e.TaskTitle != "morning sweep" {
				t.Errorf("the scheduled event does not name the job: %+v", e)
			}
		}
	}
	if !found {
		t.Error("a scheduled fire left no scheduled event")
	}
}

// book stores a schedule and hands it back, failing the test if the store
// refuses it.
func book(t *testing.T, sup *Supervisor, sc Schedule) Schedule {
	t.Helper()
	added, err := sup.Schedules().Add(sc)
	if err != nil {
		t.Fatal(err)
	}
	return added
}

// due books a schedule that is already overdue.
func due(t *testing.T, sup *Supervisor, expr string) Schedule {
	t.Helper()
	return book(t, sup, Schedule{Name: "sweep", Agent: BossID,
		Task: "check the deploy", Expr: expr, NextRunAt: time.Now().Add(-time.Minute)})
}

// The basic contract: a due schedule reaches the agent, and moves on.
func TestSweepFiresADueScheduleAndAdvancesIt(t *testing.T) {
	sup := newTestSupervisor(t)
	sc := due(t, sup, "every 30m")
	now := time.Now()

	sup.sweep(now)

	after := sup.Schedules().List()[0]
	if !after.NextRunAt.After(now) {
		t.Errorf("next run %v is not in the future", after.NextRunAt)
	}
	if after.Fires != 1 {
		t.Errorf("fires = %d, want 1", after.Fires)
	}
	if !loggedType(agentOf(t, sup, sc.Agent), "scheduled") {
		t.Error("the fire left no scheduled event in the agent's transcript")
	}
}

// Advancing at fire time is what stops a second tick re-firing the same
// occurrence.
func TestASecondSweepDoesNotRefire(t *testing.T) {
	sup := newTestSupervisor(t)
	due(t, sup, "every 30m")

	sup.sweep(time.Now())
	sup.sweep(time.Now())

	if got := sup.Schedules().List()[0].Fires; got != 1 {
		t.Errorf("fires = %d after two sweeps, want 1", got)
	}
}

// A busy agent is skipped -- but the occurrence is still spent. Leaving it due
// would bring it back every tick and deliver a burst the moment the agent frees
// up, which is the backlog the skip exists to avoid.
func TestABusyAgentIsSkippedButTheOccurrenceIsStillSpent(t *testing.T) {
	sup := newTestSupervisor(t)
	due(t, sup, "every 30m")
	a, err := sup.Get(BossID)
	if err != nil {
		t.Fatal(err)
	}
	a.setState("working")
	now := time.Now()

	sup.sweep(now)

	after := sup.Schedules().List()[0]
	if after.Fires != 0 {
		t.Errorf("fires = %d, want 0: a busy agent must not be sent work", after.Fires)
	}
	if !after.NextRunAt.After(now) {
		t.Errorf("a skipped occurrence stayed due (%v); it would repeat every tick", after.NextRunAt)
	}
	if loggedType(a, "scheduled") {
		t.Error("a busy agent was sent scheduled work anyway")
	}
}

// The whole point of the one-off form: it runs, and then it is over. A
// repeating job left standing after its one useful morning spends a turn a day
// forever, and the person has to notice and cancel it. The last run of a
// bounded series is the same case reached from the other direction: nothing
// comes after it, so the schedule is spent by it.
func TestALastRunFiresOnceAndThenRetires(t *testing.T) {
	for _, tc := range []struct{ name, expr string }{
		{"a one-off", oneOffAt(time.Now().Add(-time.Minute))},
		{"the final run before an until", lastRunToday()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup := newTestSupervisor(t)
			sc := due(t, sup, tc.expr)
			now := time.Now()

			sup.sweep(now)
			sup.sweep(now.Add(time.Minute)) // and again, the way the ticker would

			after := byID(t, sup, sc.ID)
			if after.Fires != 1 {
				t.Errorf("fires = %d after two sweeps, want exactly 1", after.Fires)
			}
			if after.Enabled {
				t.Error("a spent schedule is still enabled; it would be swept forever")
			}
			if !loggedType(agentOf(t, sup, sc.Agent), "scheduled") {
				t.Error("the fire left no scheduled event in the agent's transcript")
			}
		})
	}
}

// The opposite of the repeating case, and deliberately so. A repeating job that
// finds the agent busy spends the occurrence and waits for the next one; a run
// with nothing behind it that did the same would simply never happen. The final
// run of a bounded series gets the same protection for the same reason -- and
// there is no occurrence after it to catch the miss either.
func TestABusyAgentDoesNotSpendALastRun(t *testing.T) {
	for _, tc := range []struct{ name, expr string }{
		{"a one-off", oneOffAt(time.Now().Add(-time.Minute))},
		{"the final run before an until", lastRunToday()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup := newTestSupervisor(t)
			sc := due(t, sup, tc.expr)
			a := agentOf(t, sup, BossID)
			a.setState("working")

			sup.sweep(time.Now())

			held := byID(t, sup, sc.ID)
			if held.Fires != 0 || loggedType(a, "scheduled") {
				t.Fatal("a busy agent was sent scheduled work anyway")
			}
			if !held.Enabled || !held.NextRunAt.Equal(sc.NextRunAt) {
				t.Fatalf("the last run was spent on a busy agent: %+v", held)
			}

			a.setState("idle")
			sup.sweep(time.Now())

			if got := byID(t, sup, sc.ID); got.Fires != 1 {
				t.Errorf("fires = %d once the agent freed up, want 1: the run was dropped", got.Fires)
			}
		})
	}
}

// Retrying cannot go on forever. A reminder for an 11:00 sale is worth having at
// 11:20 and is noise by the evening, and a one-off left permanently due would be
// swept every 30s until the machine is rebuilt.
func TestAStaleOneOffIsTurnedOffRatherThanRunLate(t *testing.T) {
	sup := newTestSupervisor(t)
	missed := time.Now().Add(-oneShotGrace - time.Minute)
	sc := due(t, sup, oneOffAt(missed))
	sup.Schedules().Update(sc.ID, func(x *Schedule) { x.NextRunAt = missed })

	sup.sweep(time.Now())

	after := byID(t, sup, sc.ID)
	if after.Fires != 0 {
		t.Errorf("fires = %d, want 0: hours-late is not worth delivering", after.Fires)
	}
	if after.Enabled {
		t.Error("a one-off past its grace is still enabled; it would be swept forever")
	}
}

// A machine that was off over the instant and came back within the grace window
// should still run it -- that is the difference between "missed by four minutes"
// and a week of backlog, and it is the sweep's call, not rollForward's.
func TestRollForwardLeavesAOneOffForTheSweep(t *testing.T) {
	sup := newTestSupervisor(t)
	missed := time.Now().Add(-4 * time.Minute)
	sc := due(t, sup, oneOffAt(missed))
	sup.Schedules().Update(sc.ID, func(x *Schedule) { x.NextRunAt = missed })

	sup.rollForward(time.Now())

	held := byID(t, sup, sc.ID)
	if !held.NextRunAt.Equal(missed) {
		t.Fatalf("rollForward moved a one-off from %v to %v", missed, held.NextRunAt)
	}
	sup.sweep(time.Now())
	if got := byID(t, sup, sc.ID); got.Fires != 1 {
		t.Errorf("fires = %d, want 1: a run missed by minutes was thrown away", got.Fires)
	}
}

// oneOffAt is the expression that names a given instant, in UTC.
func oneOffAt(at time.Time) string {
	return "once on " + at.UTC().Format("2006-01-02") + " at " + at.UTC().Format("15:04")
}

// lastRunToday is a repeating expression whose window ends today, with the
// day's only run already due -- so the occurrence coming up is its last.
func lastRunToday() string {
	now := time.Now().UTC()
	return "daily at " + now.Add(-time.Minute).Format("15:04") +
		" until " + now.Format("2006-01-02") + " in UTC"
}

// "once in 2h" only means anything at the moment it is written, and Expr is
// re-read on every sweep -- stored as typed it would mean two hours from each
// sweep and never come due. It has to be resolved at creation.
func TestARelativeOneOffIsStoredAsTheInstantItNamed(t *testing.T) {
	kolkata := mustZone(t, "Asia/Kolkata")
	now := time.Date(2026, 9, 2, 9, 30, 0, 0, kolkata)

	sp, expr, at, err := planSchedule("once in 2h", kolkata, now)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(expr, "in 2h") {
		t.Errorf("expr = %q, want the instant it resolved to", expr)
	}
	if want := "once on 2026-09-02 at 11:30 in Asia/Kolkata"; expr != want {
		t.Errorf("expr = %q, want %q", expr, want)
	}
	if got := at.In(kolkata); got.Hour() != 11 || got.Minute() != 30 {
		t.Errorf("first run = %v, want 11:30 local", got)
	}
	if !sp.once || sp.rel != 0 {
		t.Errorf("the stored spec is still relative: %+v", sp)
	}
	// Re-reading the stored form on a later sweep must give the same instant,
	// which is the whole point of resolving it.
	again, err := parseSchedule(expr, kolkata)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.next(now); !got.Equal(at) {
		t.Errorf("re-reading the stored expr gives %v, want %v", got, at)
	}
}

// The resolved form names its zone, so a relative one-off does not slide because
// the person got on a plane: "in two hours" is a fixed instant, not a wall clock.
func TestARelativeOneOffDoesNotFollowThePerson(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Asia/Kolkata")
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())
	in := scheduleInput{Name: "call back", When: "once in 2h", Task: "ring them"}

	done := make(chan string, 1)
	go func() { done <- sup.CreateSchedule(context.Background(), gate, BossID, in) }()
	gate.Resolve(pendingID(t, gate), Decision{Decision: "allow"})
	<-done

	sc := sup.Schedules().List()[0]
	sup.RememberZone("America/Los_Angeles")

	if got := byID(t, sup, sc.ID).NextRunAt; !got.Equal(sc.NextRunAt) {
		t.Errorf("a relative one-off moved from %v to %v when the person did", sc.NextRunAt, got)
	}
}

// Naming a zone is how a run tied to something outside the person -- a sale
// opening at 11:00 in Kolkata -- stays put when they fly somewhere else.
func TestAPinnedScheduleDoesNotFollowThePerson(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London")
	pinned := book(t, sup, Schedule{Name: "sale", Agent: BossID, Task: "queue up",
		Expr:      "daily at 11:00 in Asia/Kolkata",
		NextRunAt: nextOf(t, "daily at 11:00 in Asia/Kolkata", mustZone(t, "Europe/London"))})
	floating := book(t, sup, Schedule{Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 11:00", NextRunAt: nextOf(t, "daily at 11:00", mustZone(t, "Europe/London"))})

	sup.RememberZone("America/Los_Angeles")

	if got := byID(t, sup, pinned.ID).NextRunAt; !got.Equal(pinned.NextRunAt) {
		t.Errorf("a pinned schedule moved from %v to %v", pinned.NextRunAt, got)
	}
	// The unpinned one is the control: without it this test would pass on a
	// rezone that had stopped working altogether.
	la := mustZone(t, "America/Los_Angeles")
	if got := byID(t, sup, floating.ID).NextRunAt.In(la); got.Hour() != 11 {
		t.Errorf("the unpinned schedule did not follow the person: %v", got)
	}
}

// A pinned schedule that is already due must survive a zone change with its
// pending run intact.
//
// The instant itself cannot move -- the expression names its own zone, so it
// recomputes to the same time either way -- but recomputing it from now would
// step over a run that was sitting due waiting for a free agent, and cancel it
// without saying so. That is the whole of what the pinned check in rezone buys.
func TestAZoneChangeDoesNotSkipAPendingPinnedRun(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London")
	overdue := time.Now().Add(-2 * time.Minute)
	pinned := book(t, sup, Schedule{Name: "sale", Agent: BossID, Task: "queue up",
		Expr: "daily at 11:00 in Asia/Kolkata", NextRunAt: overdue})
	floating := book(t, sup, Schedule{Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 11:00", NextRunAt: overdue})

	sup.RememberZone("America/Los_Angeles")

	if got := byID(t, sup, pinned.ID).NextRunAt; !got.Equal(overdue) {
		t.Errorf("a due pinned run moved from %v to %v; that occurrence is now lost",
			overdue, got)
	}
	// The control: rezone is still doing its job for schedules that should move.
	if got := byID(t, sup, floating.ID).NextRunAt; got.Equal(overdue) {
		t.Error("rezone moved nothing at all; the pinned case would pass by accident")
	}
}

// A bounded series that still has runs left behaves like any repeating job: it
// advances and stays on.
func TestASeriesInsideItsUntilKeepsGoing(t *testing.T) {
	sup := newTestSupervisor(t)
	later := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	expr := "every 30m until " + later + " in UTC"
	sc := due(t, sup, expr)
	now := time.Now()

	sup.sweep(now)

	after := byID(t, sup, sc.ID)
	if !after.Enabled {
		t.Error("a series well inside its end date was retired")
	}
	if !after.NextRunAt.After(now) {
		t.Errorf("next run %v is not in the future", after.NextRunAt)
	}
}

// An end date that has already gone by parses fine and has nothing to book, the
// same way a past one-off does.
func TestASeriesThatEndedIsRefusedWhenItIsCreated(t *testing.T) {
	_, _, at, err := planSchedule("daily at 09:00 until 2020-01-01", time.UTC, time.Now())
	if err == nil {
		t.Fatalf("a series whose end date has passed was booked for %v", at)
	}
	if !strings.Contains(err.Error(), "no run before it ends") {
		t.Errorf("error = %q, want it to say the series has already ended", err)
	}
}

// A week asleep must not wake into a week of backlog.
func TestRollForwardSkipsWhatWasMissedWhileOff(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.Schedules().Add(Schedule{Name: "sweep", Agent: BossID, Task: "check",
		Expr: "every 30m", NextRunAt: time.Now().Add(-7 * 24 * time.Hour)})
	now := time.Now()

	sup.rollForward(now)

	after := sup.Schedules().List()[0]
	if !after.NextRunAt.After(now) {
		t.Errorf("next run %v is still in the past", after.NextRunAt)
	}
	if after.Fires != 0 {
		t.Errorf("fires = %d, want 0: missed runs are skipped, not replayed", after.Fires)
	}
}

// A schedule whose agent has been deleted would otherwise fail on every tick
// forever.
func TestAScheduleForAMissingAgentTurnsItselfOff(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.Schedules().Add(Schedule{Name: "sweep", Agent: "ghost", Task: "check",
		Expr: "every 30m", NextRunAt: time.Now().Add(-time.Minute)})

	sup.sweep(time.Now())

	if sup.Schedules().List()[0].Enabled {
		t.Error("a schedule for a missing agent is still enabled")
	}
}

// agentOf starts an agent and hands it back, failing the test if it cannot.
func agentOf(t *testing.T, sup *Supervisor, id string) *Agent {
	t.Helper()
	a, err := sup.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Nothing is written until the person says yes. This is the one tool that
// commits the machine to spending money later with nobody watching, so a denial
// has to leave the store exactly as it was.
func TestSchedulingIsRefusedUntilThePersonAgrees(t *testing.T) {
	sup := newTestSupervisor(t)
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())
	in := scheduleInput{Name: "sweep", When: "every 30m", Task: "check the deploy"}

	for _, tc := range []struct {
		decision  string
		wantStore int
	}{
		{decision: "deny", wantStore: 0},
		{decision: "allow", wantStore: 1},
	} {
		done := make(chan string, 1)
		go func() { done <- sup.CreateSchedule(context.Background(), gate, BossID, in) }()
		gate.Resolve(pendingID(t, gate), Decision{Decision: tc.decision})
		answer := <-done

		if got := len(sup.Schedules().List()); got != tc.wantStore {
			t.Errorf("%s: store holds %d schedules, want %d -- answer was %q",
				tc.decision, got, tc.wantStore, answer)
		}
	}
}

// An invalid expression must be caught before the person is asked: interrupting
// someone to approve a schedule that cannot be stored wastes the one thing this
// design spends carefully.
func TestAnInvalidScheduleNeverReachesThePerson(t *testing.T) {
	sup := newTestSupervisor(t)
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())

	got := sup.CreateSchedule(context.Background(), gate,
		BossID, scheduleInput{Name: "hammer", When: "every 1m", Task: "check"})

	if !strings.Contains(got, "tightest") {
		t.Errorf("answer = %q, want it to explain the limit", got)
	}
	if events, _ := gate.log.ReadAll(); len(events) != 0 {
		t.Errorf("the person was asked about an invalid schedule: %+v", events)
	}
}

// The description is the only place a model learns what it may ask for, and it
// is where this went wrong: offered three repeating forms and nothing else, an
// agent told to act on one named day booked "daily at 05:30" and put the date
// check in its own task text. The one-off form has to be on the menu, and the
// menu has to survive tag parsing -- a comma in a jsonschema tag truncates the
// description at the comma with no error, which would silently take it back off.
func TestScheduleToolOffersTheOneOffForm(t *testing.T) {
	tools, err := scheduleTools(toolDeps{team: newTestSupervisor(t), self: BossID})
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(tools[0].InputSchema())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Properties map[string]struct{ Description string } `json:"properties"`
	}
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	when := got.Properties["when"].Description
	for _, want := range []string{"every 30m", "daily at", "weekly on", "once on", "once in",
		"every weekday", "every weekend", "monthly on", "and 17:00", "until", "between", "in Asia/Kolkata", "15m"} {
		if !strings.Contains(when, want) {
			t.Errorf("the \"when\" description does not offer %q: %q", want, when)
		}
	}
	if strings.Contains(when, ",") {
		t.Error("a comma in the jsonschema tag truncates the description the model sees")
	}
}

// A one-off is confirmed as a single run, so the model can see it does not have
// to arrange a cancellation of its own afterwards.
func TestCreateSaysWhenAOneOffRuns(t *testing.T) {
	sup := newTestSupervisor(t)
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())
	day := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	in := scheduleInput{Name: "sale", When: "once on " + day + " at 11:00", Task: "queue up"}

	done := make(chan string, 1)
	go func() { done <- sup.CreateSchedule(context.Background(), gate, BossID, in) }()
	gate.Resolve(pendingID(t, gate), Decision{Decision: "allow"})
	answer := <-done

	if !strings.Contains(answer, "Runs once at") {
		t.Errorf("answer = %q, want it to say the schedule runs once", answer)
	}
	if got := len(sup.Schedules().List()); got != 1 {
		t.Errorf("store holds %d schedules, want 1", got)
	}
}

// cancelAs runs cancel_schedule as one agent.
func cancelAs(t *testing.T, sup *Supervisor, self, id string) string {
	t.Helper()
	tools, err := scheduleTools(toolDeps{team: sup, self: self})
	if err != nil {
		t.Fatal(err)
	}
	return runTool(t, tools, "cancel_schedule", `{"id":"`+id+`"}`)[0].OfText.Text
}

// list_schedules filters by owner, so an id belonging to a colleague can only
// have arrived out of band. Cancelling on it would undo work its owner is
// relying on and never told anyone about.
func TestCancelOnlyReachesTheCallersOwnSchedules(t *testing.T) {
	sup := newTestSupervisor(t)
	sc := book(t, sup, Schedule{Name: "sweep", Agent: "colleague",
		Task: "check the deploy", Expr: "every 30m", NextRunAt: time.Now().Add(time.Hour)})

	got := cancelAs(t, sup, BossID, sc.ID)

	if !strings.Contains(got, "no schedule") {
		t.Errorf("answer = %q, want a colleague's id to read like an unknown one", got)
	}
	if len(sup.Schedules().List()) != 1 {
		t.Error("an agent cancelled a schedule belonging to someone else")
	}
	if got := cancelAs(t, sup, "colleague", sc.ID); !strings.Contains(got, "Cancelled") {
		t.Errorf("the owner could not cancel its own schedule: %q", got)
	}
}

// A person who travels must not get one firing on the zone they left. The
// stored instant is absolute, so a London 09:00 read in Los Angeles is 01:00
// local unless it is recomputed when the zone lands.
func TestAChangedZoneMovesTheClockSchedules(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London")
	daily := book(t, sup, Schedule{Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 09:00", NextRunAt: nextOf(t, "daily at 09:00", mustZone(t, "Europe/London"))})
	every := book(t, sup, Schedule{Name: "sweep", Agent: BossID, Task: "check",
		Expr: "every 30m", NextRunAt: time.Now().Add(30 * time.Minute)})

	sup.RememberZone("America/Los_Angeles")

	la := mustZone(t, "America/Los_Angeles")
	got := byID(t, sup, daily.ID).NextRunAt.In(la)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("next run = %v, want 09:00 in the zone the person moved to", got)
	}
	// An interval is not anchored to a wall clock; moving it would only push the
	// next run further out.
	if moved := byID(t, sup, every.ID).NextRunAt; !moved.Equal(every.NextRunAt) {
		t.Errorf("an \"every 30m\" schedule moved from %v to %v on a zone change",
			every.NextRunAt, moved)
	}
}

// A one-off names a wall-clock instant, so it moves with the person exactly as a
// daily does: someone who books "once on 2026-09-02 at 11:00" in London and then
// lands in Kolkata means 11:00 where they now are.
func TestAChangedZoneMovesAOneOff(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London")
	london := mustZone(t, "Europe/London")
	expr := "once on " + time.Now().In(london).AddDate(0, 0, 30).Format("2006-01-02") + " at 11:00"
	sc := book(t, sup, Schedule{Name: "sale", Agent: BossID, Task: "queue up",
		Expr: expr, NextRunAt: nextOf(t, expr, london)})

	sup.RememberZone("Asia/Kolkata")

	got := byID(t, sup, sc.ID).NextRunAt.In(mustZone(t, "Asia/Kolkata"))
	if got.Hour() != 11 || got.Minute() != 0 {
		t.Errorf("next run = %v, want 11:00 in the zone the person moved to", got)
	}
}

// A one-off whose instant has already gone by has no next run to move it to.
// Rezoning must leave the stored instant alone rather than write a zero, which
// the sweep would read as due and hold forever.
func TestAChangedZoneLeavesAPastOneOffAlone(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London")
	missed := time.Now().Add(-time.Minute)
	sc := book(t, sup, Schedule{Name: "sale", Agent: BossID, Task: "queue up",
		Expr: oneOffAt(missed), NextRunAt: missed})

	sup.RememberZone("Asia/Kolkata")

	if got := byID(t, sup, sc.ID).NextRunAt; !got.Equal(missed) {
		t.Errorf("next run moved from %v to %v; a past one-off has no next", missed, got)
	}
}

// The same zone arrives on every message, so the common case must not rewrite
// the store -- a recompute on each message would walk a daily job forward a day
// at a time and it would never fire.
func TestAnUnchangedZoneLeavesSchedulesAlone(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London")
	sc := book(t, sup, Schedule{Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 09:00", NextRunAt: nextOf(t, "daily at 09:00", mustZone(t, "Europe/London"))})

	sup.RememberZone("Europe/London")

	if got := byID(t, sup, sc.ID).NextRunAt; !got.Equal(sc.NextRunAt) {
		t.Errorf("next run moved from %v to %v with no change of zone", sc.NextRunAt, got)
	}
}

// nextOf is the first firing of an expression, for a test that needs a schedule
// already pointing at the right instant.
func nextOf(t *testing.T, expr string, loc *time.Location) time.Time {
	t.Helper()
	sp, err := parseSchedule(expr, loc)
	if err != nil {
		t.Fatal(err)
	}
	return sp.next(time.Now())
}

// byID reads back one schedule, failing the test if it has gone.
func byID(t *testing.T, sup *Supervisor, id string) Schedule {
	t.Helper()
	for _, sc := range sup.Schedules().List() {
		if sc.ID == id {
			return sc
		}
	}
	t.Fatalf("schedule %s is gone", id)
	return Schedule{}
}

// The app creates schedules directly rather than through a conversation, so the
// routes have to round-trip and to reject what the tools reject.
func TestScheduleRoutesRoundTrip(t *testing.T) {
	sup := newTestSupervisor(t)
	srv := NewServer(sup)
	// Onboarding already put the machine on a zone. The request itself says
	// nothing about where the person is, and must not need to.
	sup.RememberZone("Asia/Kolkata")

	w := do(t, srv, "POST", "/schedules",
		`{"name":"sweep","agent":"boss","task":"check the deploy","expr":"every 30m"}`)
	if w.Code != 201 {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	if got := do(t, srv, "GET", "/schedules", ""); !strings.Contains(got.Body.String(), "sweep") {
		t.Errorf("the created schedule is not listed: %s", got.Body)
	}
	if got := loadZone(sup.stateDir).String(); got != "Asia/Kolkata" {
		t.Errorf("zone = %q, want the one onboarding stored", got)
	}

	id := sup.Schedules().List()[0].ID
	if got := do(t, srv, "DELETE", "/schedules/"+id, ""); got.Code != 200 {
		t.Fatalf("delete = %d, want 200: %s", got.Code, got.Body)
	}
	if got := len(sup.Schedules().List()); got != 0 {
		t.Errorf("%d schedules survived the delete", got)
	}
	if got := do(t, srv, "DELETE", "/schedules/"+id, ""); got.Code != 404 {
		t.Errorf("deleting twice = %d, want 404", got.Code)
	}
}

// The app can book a one-off too, and the route has to store the instant the
// person's own zone makes of it rather than the guest's default UTC -- reading
// that zone off the machine, since the request no longer carries one.
func TestScheduleRoutesBookAOneOff(t *testing.T) {
	sup := newTestSupervisor(t)
	srv := NewServer(sup)
	sup.RememberZone("Asia/Kolkata")

	kolkata := mustZone(t, "Asia/Kolkata")
	day := time.Now().In(kolkata).AddDate(0, 0, 30).Format("2006-01-02")

	w := do(t, srv, "POST", "/schedules", `{"name":"sale","agent":"boss","task":"queue up",`+
		`"expr":"once on `+day+` at 11:00"}`)

	if w.Code != 201 {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	got := sup.Schedules().List()[0].NextRunAt
	if in := got.In(kolkata); in.Hour() != 11 || in.Minute() != 0 || in.Format("2006-01-02") != day {
		t.Errorf("next run = %v, want %s 11:00 in the zone onboarding stored", in, day)
	}
	if in := got.UTC(); in.Hour() != 5 || in.Minute() != 30 {
		t.Errorf("next run = %v UTC, want 05:30: the guest's default UTC was used", in)
	}
}

// The routes must not be a way around the limits the tool enforces.
func TestScheduleRoutesRejectBadInput(t *testing.T) {
	srv := NewServer(newTestSupervisor(t))
	for _, tc := range []struct{ name, body string }{
		{"too tight", `{"name":"x","agent":"boss","task":"t","expr":"every 1m"}`},
		{"not a schedule", `{"name":"x","agent":"boss","task":"t","expr":"0 9 * * *"}`},
		{"no task", `{"name":"x","agent":"boss","expr":"every 30m"}`},
		{"a date gone by", `{"name":"x","agent":"boss","task":"t","expr":"once on 2020-01-01 at 09:00"}`},
	} {
		if got := do(t, srv, "POST", "/schedules", tc.body); got.Code != 400 {
			t.Errorf("%s = %d, want 400: %s", tc.name, got.Code, got.Body)
		}
	}
	if got := do(t, srv, "POST", "/schedules",
		`{"name":"x","agent":"ghost","task":"t","expr":"every 30m"}`); got.Code != 404 {
		t.Errorf("unknown agent = %d, want 404", got.Code)
	}
}

// A machine onboarded before the zone became a name keeps its hours.
//
// The old code stored an RFC3339 stamp when a client sent no IANA name. Those
// files still exist on running machines, and dropping to UTC on the next boot
// would re-anchor every clock schedule they had booked, with no way left to say
// where the person is -- messages no longer carry a zone. Read, never written.
func TestLoadZoneStillReadsTheOldOffsetForm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(zonePath(dir), []byte("2026-08-29T14:03:11+05:30"), 0o640); err != nil {
		t.Fatal(err)
	}
	sp, err := parseSchedule("daily at 09:00", loadZone(dir))
	if err != nil {
		t.Fatal(err)
	}
	got := sp.next(time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)).UTC()
	if got.Hour() != 3 || got.Minute() != 30 {
		t.Errorf("09:00 at a stored +05:30 = %v UTC, want 03:30", got)
	}
}

// What the model is told a schedule runs at must be the person's clock.
//
// NextRunAt is an absolute instant, so rendering it raw told the model that a
// job booked for 09:00 in Kolkata runs at 03:30 -- and the model repeats that
// to the person.
func TestRenderSchedulesUsesThePersonsZone(t *testing.T) {
	kolkata := mustZone(t, "Asia/Kolkata")
	at := time.Date(2026, 9, 1, 3, 30, 0, 0, time.UTC) // 09:00 in Kolkata
	out := renderSchedules([]Schedule{{
		ID: "sch-0001", Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 09:00", NextRunAt: at, Enabled: true,
	}}, BossID, kolkata)

	if !strings.Contains(out, "09:00") {
		t.Errorf("rendered %q, want the person's 09:00 rather than the guest's UTC", out)
	}
}
