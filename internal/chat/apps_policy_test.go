package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// errNoPolicyStore stands in for Postgres being unreachable.
var errNoPolicyStore = errors.New("postgrest unavailable")

// memApps is an AppsStore in memory, so a policy can be written and read back
// without a Postgres. Locked because the routes under test read it on one
// goroutine and the push path may touch it on another.
type memApps struct {
	mu   sync.Mutex
	held agentapi.Apps
	err  error
}

func (m *memApps) Get(context.Context, string) (agentapi.Apps, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held, m.err
}

func (m *memApps) Put(_ context.Context, _ string, a agentapi.Apps) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.held = a
	return nil
}

func (m *memApps) Delete(context.Context, string) error { return nil }

// stored is what the store holds now.
func (m *memApps) stored() agentapi.Apps {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held
}

// withStore stands a server up whose provider is a stub and whose policy is kept
// in memory.
func withStore(t *testing.T, held string) (*Server, string, *memApps) {
	t.Helper()
	p := &provider{held: held}
	s, tok := p.serve(t)
	store := &memApps{}
	s.apps = store
	return s, tok, store
}

// THE test for this PR. A person sets one capability and the answer comes back
// as the whole app's row, because the screen replaces its own with what it is
// given -- a partial answer erases what it does not mention.
func TestSettingAPolicyAnswersWithTheWholeApp(t *testing.T) {
	s, tok, store := withStore(t,
		`{"items":[{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`)
	w := call(t, s, tok, "PUT", "/v1/apps/gmail/policy", `{"capability":"write","policy":"never"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got Connection
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Slug != "gmail" || got.ID != "ca_1" || got.Status != "ACTIVE" {
		t.Errorf("the row lost its connection: %+v", got)
	}
	if got.Policy["write"] != "never" {
		t.Errorf("policy came back as %v", got.Policy)
	}
	if store.stored().Policy["gmail"]["write"] != "never" {
		t.Errorf("nothing was stored: %v", store.stored().Policy)
	}
}

// One capability at a time, and the others stay. The screen sets exactly one
// segment per tap, so a write that replaced the app's map would clear whatever
// the person set a moment earlier.
func TestSettingOneCapabilityKeepsTheOthers(t *testing.T) {
	s, tok, store := withStore(t, `{"items":[]}`)
	call(t, s, tok, "PUT", "/v1/apps/gmail/policy", `{"capability":"write","policy":"auto"}`)
	call(t, s, tok, "PUT", "/v1/apps/gmail/policy", `{"capability":"del","policy":"never"}`)
	call(t, s, tok, "PUT", "/v1/apps/slack/policy", `{"capability":"write","policy":"ask"}`)

	held := store.stored().Policy
	if held["gmail"]["write"] != "auto" || held["gmail"]["del"] != "never" {
		t.Errorf("gmail lost a setting: %v", held["gmail"])
	}
	if held["slack"]["write"] != "ask" {
		t.Errorf("slack did not keep its own: %v", held["slack"])
	}
}

// Everything the screen cannot send is refused, and nothing is stored.
func TestAPolicyItCannotSetIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"a capability that is always allowed": `{"capability":"read","policy":"never"}`,
		"a capability that does not exist":    `{"capability":"launch","policy":"ask"}`,
		"an answer that does not exist":       `{"capability":"write","policy":"sometimes"}`,
		"no capability":                       `{"policy":"ask"}`,
		"no answer":                           `{"capability":"write"}`,
		"not an object":                       `"write"`,
	} {
		t.Run(name, func(t *testing.T) {
			s, tok, store := withStore(t, `{"items":[]}`)
			if w := call(t, s, tok, "PUT", "/v1/apps/gmail/policy", body); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
			if store.stored().Policy != nil {
				t.Errorf("it was stored anyway: %v", store.stored().Policy)
			}
		})
	}
}

// An app this build does not offer is refused before the store is touched, for
// the same reason connecting one is: an arbitrary slug from a client is not a
// thing to write into somebody's row.
func TestAPolicyForAnAppThisBuildDoesNotOfferIsRefused(t *testing.T) {
	s, tok, store := withStore(t, `{"items":[]}`)
	if w := call(t, s, tok, "PUT", "/v1/apps/salesforce/policy",
		`{"capability":"write","policy":"auto"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	if store.stored().Policy != nil {
		t.Errorf("it was stored anyway: %v", store.stored().Policy)
	}
}

// The connections list carries what the person chose, so the screen renders
// their settings rather than its own defaults.
func TestConnectionsCarryThePolicy(t *testing.T) {
	s, tok, store := withStore(t,
		`{"items":[{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`)
	store.held = agentapi.Apps{Policy: map[string]map[string]string{
		"gmail": {"write": "never"}}}
	w := call(t, s, tok, "GET", "/v1/apps/connections", "")
	var got []Connection
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Policy["write"] != "never" {
		t.Errorf("got %+v", got)
	}
}

// A store having a bad minute must not take the list down with it: the screen
// falls back to its own defaults for what it is not told.
func TestAnUnreadablePolicyStillListsTheConnections(t *testing.T) {
	s, tok, store := withStore(t,
		`{"items":[{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`)
	store.err = errNoPolicyStore
	w := call(t, s, tok, "GET", "/v1/apps/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got []Connection
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Policy != nil {
		t.Errorf("got %+v", got)
	}
}

// A session is minted the first time a machine is pushed to, and the permissions
// screen does not wait for that -- so a row can hold a policy with no session
// beside it. Overwriting the row with the fresh session must carry it across, or
// the person is quietly returned to defaults they had changed.
func TestMintingASessionKeepsThePolicyBesideIt(t *testing.T) {
	s, _, store := withStore(t, `{"items":[]}`)
	store.held = agentapi.Apps{Policy: map[string]map[string]string{"gmail": {"write": "never"}}}

	got, err := s.sessionFor(withTestToken("t"), testUserID)
	if err != nil {
		t.Fatalf("sessionFor: %v", err)
	}
	if got.SessionID == "" {
		t.Fatal("no session was minted")
	}
	if got.Policy["gmail"]["write"] != "never" {
		t.Errorf("the mint dropped the policy: %v", got.Policy)
	}
	if store.stored().Policy["gmail"]["write"] != "never" {
		t.Errorf("and wrote the row without it: %v", store.stored().Policy)
	}
}

// Changing a setting has to reach the machine. A pushed claim lasts as long as
// the read-only set it carried -- up to an hour -- so without dropping it the
// person changes a permission, watches nothing happen, and has no way to tell
// whether it saved.
func TestSettingAPolicyMakesTheMachineDueAnotherPush(t *testing.T) {
	s, tok, _ := withStore(t, `{"items":[]}`)
	machine := machineFor(testUserID)
	s.appsClaims[machine] = appsClaim{pushed: true, expires: time.Now().Add(time.Hour)}

	call(t, s, tok, "PUT", "/v1/apps/gmail/policy", `{"capability":"write","policy":"never"}`)

	if !s.claimApps(machine) {
		t.Error("the machine is still holding a claim, so it keeps the old policy for an hour")
	}
}
