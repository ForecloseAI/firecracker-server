package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postCreate sends a POST /vms body with the bearer token and returns the result.
func postCreate(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/vms", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// An unknown agent implementation must be rejected before a slot is allocated.
// The value reaches the guest on the kernel command line, where nothing
// validates it: a typo would boot a VM whose units both decline to start, and
// the only symptom would be a 504 sixty seconds later.
func TestCreateRejectsUnknownAgent(t *testing.T) {
	_, h := newTestServer(t)
	w := postCreate(t, h, `{"id":"alice","agent":"rust"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad_request") {
		t.Errorf("body should carry the bad_request error: %s", w.Body.String())
	}
}

// A body with no agent field must stay valid, so every client written before the
// field existed keeps working and keeps getting node.
func TestCreateAcceptsBodyWithoutAgent(t *testing.T) {
	_, h := newTestServer(t)
	w := postCreate(t, h, `{"id":"alice"}`)
	// Booting fails in a temp dir with no kernel or rootfs, but it must fail
	// LATER than validation: a 400 here would mean the field became required.
	if w.Code == http.StatusBadRequest {
		t.Errorf("omitting agent must not be a bad request: %s", w.Body.String())
	}
}
