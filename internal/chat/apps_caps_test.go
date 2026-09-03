package chat

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/composio"
)

// THE test for the resolution. Every row is a way of being uncertain, and every
// one of them has to land on asking -- this is the first thing that can make a
// machine LESS capable than intended, so nothing may reach auto by accident and
// nothing may reach never by accident either.
func TestOnlyAKnownAnswerAboutAKnownCapabilityIsObeyed(t *testing.T) {
	for name, c := range map[string]struct {
		capability, chosen, want string
	}{
		"reading is always allowed":        {composio.CapRead, "", agentapi.ActionAuto},
		"and cannot be turned off":         {composio.CapRead, agentapi.ActionNever, agentapi.ActionAuto},
		"a write they allowed":             {composio.CapWrite, agentapi.ActionAuto, agentapi.ActionAuto},
		"a write they refused":             {composio.CapWrite, agentapi.ActionNever, agentapi.ActionNever},
		"a write they said nothing about":  {composio.CapWrite, "", agentapi.ActionAsk},
		"a delete they allowed":            {composio.CapDelete, agentapi.ActionAuto, agentapi.ActionAuto},
		"an answer we do not recognise":    {composio.CapWrite, "sometimes", agentapi.ActionAsk},
		"a capability we do not recognise": {"launch", agentapi.ActionAuto, agentapi.ActionAsk},
		"nothing at all":                   {"", "", agentapi.ActionAsk},
	} {
		if got := actionFor(c.capability, c.chosen); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// stubCaps answers for any app without a provider, counting the fetches.
func stubCaps(fn func(string) (map[string]string, error)) (*appCaps, *atomic.Int64) {
	var calls atomic.Int64
	a := &appCaps{held: map[string]capsEntry{},
		fetch: func(_ context.Context, app string) (map[string]string, error) {
			calls.Add(1)
			return fn(app)
		}}
	return a, &calls
}

// connected is the apps a stubbed person holds, which is what resolution now
// runs over: the featured list is no longer the set, because an action in an app
// nobody has connected cannot run whatever we say about it.
var connected = []string{"gmail", "slack"}

// A person's answer reaches the actions of the app they gave it about, and no
// others. The screen sets one app at a time.
func TestAPolicyReachesOnlyItsOwnApp(t *testing.T) {
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		return map[string]string{app + "_GET": composio.CapRead,
			app + "_SEND": composio.CapWrite, app + "_DROP": composio.CapDelete}, nil
	})
	got, _ := a.resolved(context.Background(), connected, map[string]map[string]string{
		"gmail": {composio.CapWrite: agentapi.ActionNever},
	})
	if got["gmail_SEND"] != agentapi.ActionNever {
		t.Errorf("gmail_SEND is %q", got["gmail_SEND"])
	}
	if got["gmail_GET"] != agentapi.ActionAuto {
		t.Errorf("a read is %q", got["gmail_GET"])
	}
	// Absent rather than "ask", here and below. The guest reads an unknown slug
	// as ask, so the two say exactly the same thing -- and saying it by leaving
	// the entry out is what keeps a person's whole connected surface inside the
	// push the guest will accept.
	if _, sent := got["slack_SEND"]; sent {
		t.Errorf("slack_SEND is %q, so one app's answer leaked into another", got["slack_SEND"])
	}
	if _, sent := got["gmail_DROP"]; sent {
		t.Errorf("a delete with no answer was pushed as %q", got["gmail_DROP"])
	}
}

// THE test for what keeps the push inside the guest's ceiling. Everything that
// resolves to asking is left out, and the guest reads absence as ask -- so this
// is the same policy in half the bytes, and the bytes are the thing with a limit
// (appsBodyCap, in internal/agentd/routes_apps.go, which takes the SESSION down
// with the set when it is exceeded).
func TestNothingThatWouldOnlyAskIsPushed(t *testing.T) {
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		return map[string]string{
			app + "_GET": composio.CapRead, app + "_SEND": composio.CapWrite,
			app + "_DROP": composio.CapDelete}, nil
	})
	got, _ := a.resolved(context.Background(), []string{"gmail"}, nil)
	if !maps.Equal(got, map[string]string{"gmail_GET": agentapi.ActionAuto}) {
		t.Errorf("got %v, want only the read -- the write and the delete both ask", got)
	}
}

// A person's connected apps decide what is resolved, not a list written down
// here. Resolving an app nobody has connected spends a round trip and a slice of
// a bounded push on actions that cannot run.
func TestOnlyTheAppsAskedAboutAreFetched(t *testing.T) {
	var asked []string
	var mu sync.Mutex
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		mu.Lock()
		asked = append(asked, app)
		mu.Unlock()
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	a.resolved(context.Background(), []string{"notion"}, nil)
	if !slices.Equal(asked, []string{"notion"}) {
		t.Errorf("asked about %v, want only the app that was connected", asked)
	}
}

// One app's answer is cached on its own, so a person connecting a second app
// does not cost a re-fetch of the first.
func TestOneAppsActionsAreCachedWithoutTheOthers(t *testing.T) {
	a, calls := stubCaps(func(app string) (map[string]string, error) {
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	a.resolved(context.Background(), []string{"gmail"}, nil)
	calls.Store(0)
	got, _ := a.resolved(context.Background(), []string{"gmail", "notion"}, nil)
	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d apps, want only the new one", n)
	}
	if got["gmail_GET"] == "" || got["notion_GET"] == "" {
		t.Errorf("got %v, want both apps in the answer", got)
	}
}

// An action we disagree with the provider about is reclassified before anything
// is cached, so no later consumer can forget to.
func TestAnActionWeDisagreeAboutIsNotARead(t *testing.T) {
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		if app == "gmail" {
			return map[string]string{"GMAIL_FETCH_EMAILS": composio.CapRead,
				"GMAIL_CREATE_PROMPT_POST": composio.CapRead}, nil
		}
		return nil, nil
	})
	got, _ := a.resolved(context.Background(), connected, nil)
	if _, sent := got["GMAIL_CREATE_PROMPT_POST"]; sent {
		t.Errorf("it resolved to %q, so it runs unasked", got["GMAIL_CREATE_PROMPT_POST"])
	}
	if got["GMAIL_FETCH_EMAILS"] != agentapi.ActionAuto {
		t.Error("a genuine read was taken with it")
	}
}

// Fetched once and kept, so pushing to a machine does not cost six round trips
// every time one boots.
func TestTheCapabilityMapIsFetchedOncePerTTL(t *testing.T) {
	a, calls := stubCaps(func(app string) (map[string]string, error) {
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	for range 3 {
		a.resolved(context.Background(), connected, nil)
	}
	if n := calls.Load(); n != int64(len(connected)) {
		t.Errorf("fetched %d times, want %d", n, len(connected))
	}
}

// An app that did not answer contributes nothing, so its actions ask -- and the
// partial answer is not cached, because an hour of asking about ordinary reads
// is how a gate teaches people to stop reading it.
func TestAnAppThatDidNotAnswerContributesNoCapabilities(t *testing.T) {
	down := true
	a, calls := stubCaps(func(app string) (map[string]string, error) {
		if down && app == "slack" {
			return nil, errors.New("provider had a bad minute")
		}
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	got, until := a.resolved(context.Background(), connected, nil)
	if _, ok := got["slack_GET"]; ok {
		t.Error("an app that failed to answer contributed actions")
	}
	if d := time.Until(until); d > appsRetry {
		t.Errorf("an outage's answer is good for %v, longer than the short clock", d)
	}

	down = false
	calls.Store(0)
	got, until = a.resolved(context.Background(), connected, nil)
	if calls.Load() == 0 {
		t.Fatal("a partial map was cached, so slack keeps asking for an hour")
	}
	if got["slack_GET"] != agentapi.ActionAuto {
		t.Error("the retry did not pick up the app that had recovered")
	}
	// The other half of the same clock: a WHOLE answer is kept for the hour. Only
	// pinning the short one would pass with both clocks set to five minutes, which
	// re-fans-out across six apps twelve times an hour forever.
	if d := time.Until(until); d < appCapsTTL-time.Second {
		t.Errorf("a complete answer is good for only %v, so every machine "+
			"re-fetches far more often than the provider ships", d)
	}
}

// The whole point of resolving host-side: the guest is handed one flat answer
// per action and holds no vocabulary of its own.
func TestTheAnswerIsFlatAndPerAction(t *testing.T) {
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		if app == "gmail" {
			return map[string]string{"GMAIL_SEND_EMAIL": composio.CapWrite}, nil
		}
		return nil, nil
	})
	got, _ := a.resolved(context.Background(), connected, map[string]map[string]string{
		"gmail": {composio.CapWrite: agentapi.ActionAuto}})
	if !maps.Equal(got, map[string]string{"GMAIL_SEND_EMAIL": agentapi.ActionAuto}) {
		t.Errorf("got %v", got)
	}
}

// The push carries the resolved answer, resolved against THIS person's policy.
// Everything above is about producing it; this is the only test that it leaves
// the host.
func TestThePushCarriesTheResolvedActions(t *testing.T) {
	s, cl, body := pushingServer(t)
	s.kinds, _ = stubCaps(func(app string) (map[string]string, error) {
		if app == "gmail" {
			return map[string]string{"GMAIL_SEND_EMAIL": composio.CapWrite}, nil
		}
		return nil, nil
	})
	s.apps = &heldAppsStore{held: agentapi.Apps{
		SessionURL: "https://backend.composio.dev/mcp/sess_1", SessionID: "sess_1",
		Policy: map[string]map[string]string{"gmail": {composio.CapWrite: agentapi.ActionNever}}}}

	if _, err := s.pushApps(context.Background(), testUserID,
		vmView{ID: "m1", GuestIP: "127.0.0.1"}, cl); err != nil {
		t.Fatalf("push failed: %v", err)
	}
	var got agentapi.Apps
	if err := json.Unmarshal(*body, &got); err != nil {
		t.Fatalf("body %q: %v", *body, err)
	}
	if got.Actions["GMAIL_SEND_EMAIL"] != agentapi.ActionNever {
		t.Errorf("the machine was told %q about a send this person refused", got.Actions["GMAIL_SEND_EMAIL"])
	}
}

// The push comes due on the EARLIER of the two clocks. Each cache decides on its
// own whether its answer was whole, so an outage at the capability map alone
// leaves a complete read-only set on the hour beside an Actions map whose gaps
// all ask. Taking the set's deadline would hold that for an hour with nothing
// bringing it back early -- and it is invisible until a guest obeys Actions.
func TestAPartialCapabilityMapShortensThePushDeadline(t *testing.T) {
	s, cl, _ := pushingServer(t)
	s.kinds, _ = stubCaps(func(app string) (map[string]string, error) {
		if app == "gmail" {
			return nil, errors.New("provider had a bad minute")
		}
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	done, err := s.pushApps(context.Background(), testUserID,
		vmView{ID: "m1", GuestIP: "127.0.0.1"}, cl)
	if err != nil {
		t.Fatalf("push failed: %v", err)
	}
	if wait := time.Until(done.until); wait > appsRetry+time.Second {
		t.Errorf("a machine holding a partial capability map is due again in %v, "+
			"so every action missing from it asks for the whole hour", wait)
	}
}
