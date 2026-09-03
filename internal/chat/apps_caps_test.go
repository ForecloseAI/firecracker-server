package chat

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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
	a := &appCaps{fetch: func(_ context.Context, app string) (map[string]string, error) {
		calls.Add(1)
		return fn(app)
	}}
	return a, &calls
}

// A person's answer reaches the actions of the app they gave it about, and no
// others. The screen sets one app at a time.
func TestAPolicyReachesOnlyItsOwnApp(t *testing.T) {
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		return map[string]string{app + "_GET": composio.CapRead,
			app + "_SEND": composio.CapWrite, app + "_DROP": composio.CapDelete}, nil
	})
	got, _ := a.resolved(context.Background(), map[string]map[string]string{
		"gmail": {composio.CapWrite: agentapi.ActionNever},
	})
	if got["gmail_SEND"] != agentapi.ActionNever {
		t.Errorf("gmail_SEND is %q", got["gmail_SEND"])
	}
	if got["slack_SEND"] != agentapi.ActionAsk {
		t.Errorf("slack_SEND is %q, so one app's answer leaked into another", got["slack_SEND"])
	}
	if got["gmail_GET"] != agentapi.ActionAuto {
		t.Errorf("a read is %q", got["gmail_GET"])
	}
	if got["gmail_DROP"] != agentapi.ActionAsk {
		t.Errorf("a delete with no answer is %q", got["gmail_DROP"])
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
	got, _ := a.resolved(context.Background(), nil)
	if got["GMAIL_CREATE_PROMPT_POST"] != agentapi.ActionAsk {
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
		a.resolved(context.Background(), nil)
	}
	if n := calls.Load(); n != int64(len(featured)) {
		t.Errorf("fetched %d times, want %d", n, len(featured))
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
	got, until := a.resolved(context.Background(), nil)
	if _, ok := got["slack_GET"]; ok {
		t.Error("an app that failed to answer contributed actions")
	}
	if d := time.Until(until); d > appsRetry {
		t.Errorf("an outage's answer is good for %v, longer than the short clock", d)
	}

	down = false
	calls.Store(0)
	if got, _ = a.resolved(context.Background(), nil); calls.Load() == 0 {
		t.Fatal("a partial map was cached, so slack keeps asking for an hour")
	}
	if got["slack_GET"] != agentapi.ActionAuto {
		t.Error("the retry did not pick up the app that had recovered")
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
	got, _ := a.resolved(context.Background(), map[string]map[string]string{
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
	until, err := s.pushApps(context.Background(), testUserID,
		vmView{ID: "m1", GuestIP: "127.0.0.1"}, cl)
	if err != nil {
		t.Fatalf("push failed: %v", err)
	}
	if wait := time.Until(until); wait > appsRetry+time.Second {
		t.Errorf("a machine holding a partial capability map is due again in %v, "+
			"so every action missing from it asks for the whole hour", wait)
	}
}
