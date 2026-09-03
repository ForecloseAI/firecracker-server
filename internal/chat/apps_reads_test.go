package chat

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// stubReads answers for any app without a provider, counting the fetches.
func stubReads(fn func(string) ([]string, error)) (*appReads, *atomic.Int64) {
	var calls atomic.Int64
	a := &appReads{fetch: func(_ context.Context, slug string) ([]string, error) {
		calls.Add(1)
		return fn(slug)
	}}
	return a, &calls
}

// The set is fetched once and then kept, so pushing a session to a machine does
// not cost six round trips every time one boots.
func TestTheReadOnlySetIsFetchedOncePerTTL(t *testing.T) {
	a, calls := stubReads(func(slug string) ([]string, error) {
		return []string{slug + "_GET"}, nil
	})
	for range 3 {
		if got, _ := a.slugs(context.Background()); len(got) != len(featured) {
			t.Fatalf("got %d, want one per app", len(got))
		}
	}
	if n := calls.Load(); n != int64(len(featured)) {
		t.Errorf("fetched %d times, want %d -- the cache is not holding", n, len(featured))
	}
}

// THE test for this PR. An app that did not answer contributes nothing, so its
// tools fall outside the set and ask a person -- noisy, never permissive. And
// the partial answer is NOT cached, because an hour of asking about ordinary
// reads is how a gate teaches people to stop reading it.
func TestAnAppThatDidNotAnswerIsAbsentAndNotCached(t *testing.T) {
	down := true
	a, calls := stubReads(func(slug string) ([]string, error) {
		if down && slug == "slack" {
			return nil, errors.New("provider had a bad minute")
		}
		return []string{slug + "_GET"}, nil
	})

	got, _ := a.slugs(context.Background())
	if slices.Contains(got, "slack_GET") {
		t.Fatal("an app that failed to answer contributed slugs")
	}
	if len(got) != len(featured)-1 {
		t.Errorf("got %d, want every app but the one that failed", len(got))
	}

	down = false
	calls.Store(0)
	if got, _ = a.slugs(context.Background()); calls.Load() == 0 {
		t.Fatal("a partial answer was cached for an hour, so slack's reads keep asking")
	}
	if !slices.Contains(got, "slack_GET") {
		t.Error("the retry did not pick up the app that had recovered")
	}
}

// A stale set is refreshed rather than served forever: the point of not
// compiling this in is that a tool shipped today is understood within the hour.
func TestAStaleReadOnlySetIsRefetched(t *testing.T) {
	a, calls := stubReads(func(slug string) ([]string, error) { return []string{slug + "_GET"}, nil })
	a.slugs(context.Background())
	a.mu.Lock()
	a.expires = time.Now().Add(-time.Second)
	a.mu.Unlock()
	calls.Store(0)
	a.slugs(context.Background())
	if calls.Load() == 0 {
		t.Error("a stale set was served without refreshing")
	}
}

// A provider that answers "nothing here" is a real answer and is cached as one.
// Distinguishing it from "not fetched yet" is why fresh returns a bool: without
// that, an empty set would re-fetch six times on every push forever.
func TestAnEmptyAnswerIsStillAnAnswer(t *testing.T) {
	a, calls := stubReads(func(string) ([]string, error) { return nil, nil })
	a.slugs(context.Background())
	calls.Store(0)
	if got, _ := a.slugs(context.Background()); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if calls.Load() != 0 {
		t.Errorf("re-fetched %d times for an answer it already had", calls.Load())
	}
}

// THE test for the overlay. GMAIL_CREATE_PROMPT_POST is tagged readOnlyHint by
// the provider and posts text to an unrelated third party, so it must never
// reach a machine as safe -- and asserted on what is PUSHED, not on what the
// fetch returned, or the subtraction can be reintroduced one layer lower.
func TestAnActionWeDisagreeAboutIsNeverPushedAsSafe(t *testing.T) {
	a, _ := stubReads(func(slug string) ([]string, error) {
		if slug == "gmail" {
			return []string{"GMAIL_FETCH_EMAILS", "GMAIL_CREATE_PROMPT_POST"}, nil
		}
		return []string{slug + "_GET"}, nil
	})
	got, _ := a.slugs(context.Background())
	if slices.Contains(got, "GMAIL_CREATE_PROMPT_POST") {
		t.Error("a tool we do not accept as read-only was pushed as read-only")
	}
	if !slices.Contains(got, "GMAIL_FETCH_EMAILS") {
		t.Error("the overlay took a genuine read with it")
	}
}
