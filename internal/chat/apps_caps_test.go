package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
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

// connected is the apps these tests treat as this person's.
var connected = []string{"gmail", "slack"}

// stubCaps answers for any app without a provider, counting the fetches.
func stubCaps(fn func(string) (map[string]string, error)) (*appCaps, *atomic.Int64) {
	var calls atomic.Int64
	a := &appCaps{held: map[string]capEntry{},
		fetch: func(_ context.Context, app string) (map[string]string, error) {
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
	got, _ := a.resolved(context.Background(), connected, map[string]map[string]string{
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

// THE test for resolving against what somebody CONNECTED rather than what this
// build offers. The catalogue is over a hundred apps: fanning out across all of
// them would be a hundred round trips for an answer no machine could be pushed,
// and an app nobody has connected has no actions worth classifying.
func TestOnlyConnectedAppsCostAFetch(t *testing.T) {
	var asked []string
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		asked = append(asked, app)
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	got, _ := a.resolved(context.Background(), []string{"gmail"}, nil)
	if len(asked) != 1 || asked[0] != "gmail" {
		t.Errorf("asked the provider about %q", asked)
	}
	if _, ok := got["notion_GET"]; ok {
		t.Error("an app nobody connected contributed actions")
	}
	// And absent is what the guest reads as asking, which is the whole reason
	// this is safe to narrow.
	if got["notion_GET"] != "" {
		t.Errorf("an unconnected app resolved to %q", got["notion_GET"])
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
	if got["GMAIL_CREATE_PROMPT_POST"] != agentapi.ActionAsk {
		t.Errorf("it resolved to %q, so it runs unasked", got["GMAIL_CREATE_PROMPT_POST"])
	}
	if got["GMAIL_FETCH_EMAILS"] != agentapi.ActionAuto {
		t.Error("a genuine read was taken with it")
	}
}

// Fetched once per app and kept, so pushing to a machine does not cost a round
// trip per connected app every time one boots -- and two people who connected
// the same app share the entry.
func TestTheCapabilityMapIsFetchedOncePerTTL(t *testing.T) {
	a, calls := stubCaps(func(app string) (map[string]string, error) {
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	for range 3 {
		a.resolved(context.Background(), connected, nil)
	}
	if n := calls.Load(); n != int64(len(connected)) {
		t.Errorf("fetched %d times, want one per app (%d)", n, len(connected))
	}
}

// An app that did not answer contributes nothing, so its actions ask -- and its
// failure is not cached for the hour, because an hour of asking about ordinary
// reads is how a gate teaches people to stop reading it.
//
// Cached PER APP now, so this also pins that one app's bad minute does not throw
// away the apps that answered beside it.
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
	if got["gmail_GET"] != agentapi.ActionAuto {
		t.Error("the app that answered was thrown away with the one that did not")
	}
	if d := time.Until(until); d > appsRetry {
		t.Errorf("an outage's answer is good for %v, longer than the short clock", d)
	}

	down = false
	calls.Store(0)
	got, until = a.resolved(context.Background(), connected, nil)
	if calls.Load() != 1 {
		t.Fatalf("the retry made %d calls, want only the app that failed", calls.Load())
	}
	if got["slack_GET"] != agentapi.ActionAuto {
		t.Error("the retry did not pick up the app that had recovered")
	}
	// The other half of the same clock: a WHOLE answer is kept for the hour. Only
	// pinning the short one would pass with both clocks set to five minutes.
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
	got, _ := a.resolved(context.Background(), []string{"gmail"}, map[string]map[string]string{
		"gmail": {composio.CapWrite: agentapi.ActionAuto}})
	if !maps.Equal(got, map[string]string{"GMAIL_SEND_EMAIL": agentapi.ActionAuto}) {
		t.Errorf("got %v", got)
	}
}

// THE test for the budget, and the failure it exists to avoid is total.
//
// The guest refuses a body over its own cap and answers 400 BEFORE writing the
// file, so an oversized push does not lose the set -- it takes that machine's
// whole app session down, deterministically, on every retry. Dropping whole
// apps degrades to asking, which is noisy and safe; overshooting degrades to no
// connected apps at all until somebody reads a log.
func TestAnOversizedAnswerDropsWholeAppsRatherThanThePush(t *testing.T) {
	a, _ := stubCaps(func(app string) (map[string]string, error) {
		out := map[string]string{}
		for i := range map[string]int{"gmail": 200, "github": 12000}[app] {
			out[fmt.Sprintf("%s_ACTION_%d", strings.ToUpper(app), i)] = composio.CapRead
		}
		return out, nil
	})
	got, _ := a.resolved(context.Background(), []string{"github", "gmail"}, nil)
	if pushBytes(map[string]map[string]string{"": got}) > appsActionBytes {
		t.Fatalf("the answer is %d bytes, over the budget the guest will refuse",
			pushBytes(map[string]map[string]string{"": got}))
	}
	if got["GMAIL_ACTION_0"] != agentapi.ActionAuto {
		t.Error("the small app was dropped instead of the large one")
	}
	if _, ok := got["GITHUB_ACTION_0"]; ok {
		t.Error("the app that did not fit was kept anyway")
	}
	// Whole apps rather than a truncation: half an app is somebody asked about
	// some of its reads and not others, for no reason they could see.
	for slug := range got {
		if strings.HasPrefix(slug, "GITHUB_") {
			t.Fatalf("%s survived, so an app was cut in half", slug)
		}
	}
}

// The push carries the resolved answer, resolved against THIS person's policy.
// Everything above is about producing it; this is the only test that it leaves
// the host.
func TestThePushCarriesTheResolvedActions(t *testing.T) {
	s, cl, body := pushingServer(t, `{"items":[
		{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`)
	s.kinds, _ = stubCaps(func(app string) (map[string]string, error) {
		if app == "gmail" {
			return map[string]string{"GMAIL_SEND_EMAIL": composio.CapWrite}, nil
		}
		return nil, nil
	})
	s.apps = &heldAppsStore{held: agentapi.Apps{
		SessionURL: "https://backend.composio.dev/mcp/sess_1", SessionID: "sess_1",
		Policy: map[string]map[string]string{"gmail": {composio.CapWrite: agentapi.ActionNever}}}}

	if _, _, err := s.pushApps(context.Background(), testUserID,
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
// leaves a complete catalogue on the fortnight beside an Actions map whose gaps
// all ask. Taking the catalogue's deadline would hold that for two weeks with
// nothing bringing it back early -- and it is invisible until a guest obeys
// Actions.
func TestAPartialCapabilityMapShortensThePushDeadline(t *testing.T) {
	s, cl, _ := pushingServer(t, `{"items":[
		{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`)
	s.kinds, _ = stubCaps(func(app string) (map[string]string, error) {
		if app == "gmail" {
			return nil, errors.New("provider had a bad minute")
		}
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	until, _, err := s.pushApps(context.Background(), testUserID,
		vmView{ID: "m1", GuestIP: "127.0.0.1"}, cl)
	if err != nil {
		t.Fatalf("push failed: %v", err)
	}
	if wait := time.Until(until); wait > appsRetry+time.Second {
		t.Errorf("a machine holding a partial capability map is due again in %v, "+
			"so every action missing from it asks for the whole hour", wait)
	}
}
