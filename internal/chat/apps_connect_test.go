package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Connecting a featured app hands back a link and the deadline it dies on.
func TestConnectMintsALinkWithItsDeadline(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	w := call(t, s, tok, "POST", "/v1/apps/gmail/connect", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got ConnectLink
	json.Unmarshal(w.Body.Bytes(), &got)
	if !strings.HasPrefix(got.RedirectURL, "https://connect.composio.dev/") {
		t.Errorf("link is %q", got.RedirectURL)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("no deadline, so a stale link cannot be told from a fresh one")
	}
}

// The provider would mint a link for any of its thousand-odd apps, and the
// catalogue is now what says which of those this build will put somebody
// through. A slug that is not in it never reaches the provider -- which matters
// because the call it would reach CREATES an auth config, project-wide and
// permanent, for an app nobody chose to offer.
func TestConnectRefusesAnAppThisBuildDoesNotOffer(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	for _, slug := range []string{"salesforce", "", "../../etc"} {
		w := call(t, s, tok, "POST", "/v1/apps/"+slug+"/connect", "")
		if w.Code == http.StatusCreated {
			t.Errorf("%q was connected", slug)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.linked) != 0 {
		t.Errorf("the provider was asked anyway: %v", p.linked)
	}
}

// THE test for this PR. Ids are the only thing standing between one person's
// accounts and another's, so a delete must resolve the id against the caller's
// own connections first -- otherwise anybody holding an id revokes a stranger's
// Gmail.
func TestDisconnectRefusesAnIdThatIsNotYours(t *testing.T) {
	p := &provider{held: `{"items":[{"id":"ca_mine","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`}
	s, tok := p.serve(t)

	w := call(t, s, tok, "DELETE", "/v1/apps/connections/ca_someone_elses", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.deleted) != 0 {
		t.Fatalf("a stranger's account was revoked: %v", p.deleted)
	}
}

// Their own account disconnects, and the provider is actually told.
func TestDisconnectRevokesYourOwnAccount(t *testing.T) {
	p := &provider{held: `{"items":[{"id":"ca_mine","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`}
	s, tok := p.serve(t)

	if w := call(t, s, tok, "DELETE", "/v1/apps/connections/ca_mine", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.deleted) != 1 || p.deleted[0] != "ca_mine" {
		t.Errorf("revoked %v", p.deleted)
	}
}

// An account already gone answers 404, which the client deliberately reads as
// success: the row is where they wanted it either way.
func TestDisconnectingSomethingAlreadyGoneIs404(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	if w := call(t, s, tok, "DELETE", "/v1/apps/connections/ca_gone", ""); w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}
