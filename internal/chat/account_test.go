package chat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeControl records what the gateway asked the control plane to do, and can be
// told to refuse. The shared stubControl answers every path with a VM view, so a
// delete needs one that routes.
type fakeControl struct {
	mu      sync.Mutex
	deleted []string
	purged  []string
	fail    bool
	// notFound answers 404, as a control plane deployed before the purge fix
	// does for a machine that is not running -- while its workspace stays on
	// disk.
	notFound bool
	// conflict answers 409, as the control plane does when the machine was
	// replaced before the purge could reach it.
	conflict bool
}

func (f *fakeControl) server(t *testing.T) *Control {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Write([]byte(`{"id":"x","state":"running","guest_ip":"127.0.0.1"}`))
			return
		}
		f.mu.Lock()
		id := strings.TrimPrefix(r.URL.Path, "/vms/")
		f.deleted = append(f.deleted, id)
		if r.URL.Query().Get("purge") == "true" {
			f.purged = append(f.purged, id)
		}
		bad, missing, clash := f.fail, f.notFound, f.conflict
		f.mu.Unlock()
		if clash {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"conflict"}`))
			return
		}
		if missing {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		if bad {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":"vm_unreachable"}`))
			return
		}
		w.Write([]byte(`{"id":"` + id + `","purged":true}`))
	}))
	t.Cleanup(srv.Close)
	return NewControl(srv.URL, "fleet-token")
}

// accountServer wires a gateway whose control plane is the recorder.
func accountServer(t *testing.T) (*Server, *fakeControl, string) {
	t.Helper()
	fc := &fakeControl{}
	v, mint := testAuth(t)
	s := &Server{
		control: fc.server(t), auth: v,
		caps:    NewCaps("https://vnc.example.com"),
		bridges: map[string]*Bridge{},
		cfg:     Config{Origin: "https://chat.example.com", Token: "fleet-token"},
	}
	return s, fc, mint(testUserID, "tester@example.com")
}

// testMachine is the machine id derived from testUserID.
const testMachine = "3f8a1c925e4b4d7a9c110b2e6f8a4d31"

// The whole promise of the feature: the caller's own machine is deleted, with
// the workspace purged rather than merely stopped.
func TestDeleteAccountPurgesTheCallersMachine(t *testing.T) {
	s, fc, tok := accountServer(t)
	w := call(t, s, tok, "DELETE", "/v1/account", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleted) != 1 || fc.deleted[0] != testMachine {
		t.Fatalf("deleted = %v, want [%s]", fc.deleted, testMachine)
	}
	if len(fc.purged) != 1 {
		t.Error("the machine was stopped but its data was not purged")
	}
}

// A failure must not read as success. The app tells someone their data is gone
// on the strength of this status.
func TestDeleteAccountReportsAFailure(t *testing.T) {
	s, fc, tok := accountServer(t)
	fc.fail = true
	if w := call(t, s, tok, "DELETE", "/v1/account", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// The machine id is derived from the account, so the replacement machine reuses
// it. A bridge left behind holds the old event watermark and would swallow the
// new machine's transcript until its ids passed it.
func TestDeleteAccountDropsTheBridge(t *testing.T) {
	s, _, tok := accountServer(t)
	s.bridge(testMachine) // as the operator page would have created it
	if _, ok := s.bridges[testMachine]; !ok {
		t.Fatal("the bridge was not created")
	}
	if w := call(t, s, tok, "DELETE", "/v1/account", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if _, ok := s.bridges[testMachine]; ok {
		t.Error("the deleted machine's bridge is still cached")
	}
}

// A handoff capability lives 15 minutes and only checks key, VM id and expiry.
// Since the replacement machine reuses the id, one minted before the wipe would
// otherwise open the screen of the machine after it.
func TestDeleteAccountRevokesScreenAccess(t *testing.T) {
	s, _, tok := accountServer(t)
	url := s.caps.Mint(testMachine)
	key := url[strings.Index(url, "?k=")+3:]
	if !s.caps.check(key, testMachine) {
		t.Fatal("the capability was not minted")
	}
	if w := call(t, s, tok, "DELETE", "/v1/account", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if s.caps.check(key, testMachine) {
		t.Error("a capability minted before the wipe still opens the new machine")
	}
}

// Revoking must be surgical: another account's screen access is not the caller's
// to end.
func TestDeleteAccountLeavesOtherMachinesAlone(t *testing.T) {
	s, fc, tok := accountServer(t)
	other := s.caps.Mint("someone-elses-machine")
	otherKey := other[strings.Index(other, "?k=")+3:]
	if w := call(t, s, tok, "DELETE", "/v1/account", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if !s.caps.check(otherKey, "someone-elses-machine") {
		t.Error("another account's capability was revoked")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, id := range fc.deleted {
		if id != testMachine {
			t.Errorf("deleted %q, which is not the caller's machine", id)
		}
	}
}

// A 404 must not read as "already gone".
//
// The fixed control plane never answers one to a purge: it erases a stopped
// machine's workspace and reports 200 whether or not there was anything there.
// So a 404 means the old control plane is still deployed -- the one that leaves
// the workspace on disk -- and answering 204 would tell someone their data was
// erased during exactly the rolling deploy where it was not.
func TestDeleteAccountDoesNotTreatNotFoundAsSuccess(t *testing.T) {
	s, fc, tok := accountServer(t)
	fc.notFound = true
	w := call(t, s, tok, "DELETE", "/v1/account", "")
	if w.Code == http.StatusNoContent {
		t.Fatal("a 404 from the control plane was reported as data erased")
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// A subscriber racing the delete must not bring the bridge back.
//
// Subscribe treats a cancelled context as an idle stop and revives from it, and
// cancelling is exactly what closing does -- so without a terminal flag the
// consumer dropping the bridge was meant to remove returns, still holding the
// deleted machine's event watermark, to reconnect to the replacement under the
// same id.
func TestADeletedBridgeCannotBeRevived(t *testing.T) {
	s, _, tok := accountServer(t)
	b := s.bridge(testMachine)
	if w := call(t, s, tok, "DELETE", "/v1/account", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	// As an /api/stream request that grabbed the pointer before the delete would.
	ch := b.Subscribe()
	b.mu.Lock()
	revived := b.ctx.Err() == nil
	subs := len(b.subs)
	b.mu.Unlock()
	if revived {
		t.Error("the deleted machine's consumer reconnected")
	}
	if subs != 0 {
		t.Errorf("the deleted bridge kept %d subscriber(s)", subs)
	}
	if _, open := <-ch; open {
		t.Error("a deleted bridge delivered a frame")
	}
}

// A purge the control plane could not carry out must not read as erased.
//
// It answers 409 when the machine was replaced before the purge reached it --
// the replacement and its workspace are untouched, so 204 here would claim an
// erasure that did not happen. The app shows a failure and the person retries,
// which then targets the machine that is actually there.
func TestDeleteAccountDoesNotTreatAConflictAsSuccess(t *testing.T) {
	s, fc, tok := accountServer(t)
	fc.conflict = true
	w := call(t, s, tok, "DELETE", "/v1/account", "")
	if w.Code == http.StatusNoContent {
		t.Fatal("a purge the control plane refused was reported as data erased")
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}
