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

// A caller still sending the retired `agent` field must not break. Nothing in
// the guest can act on it any more, and rejecting it would turn a harmless stale
// client into a hard failure for no gain -- encoding/json drops unknown fields,
// and this pins that it stays that way.
func TestCreateIgnoresTheRetiredAgentField(t *testing.T) {
	_, h := newTestServer(t)
	w := postCreate(t, h, `{"id":"alice","agent":"go"}`)
	// Booting fails in a temp dir with no kernel or rootfs, but it must fail
	// LATER than validation: a 400 here would mean the field became meaningful.
	if w.Code == http.StatusBadRequest {
		t.Errorf("a stale agent field must not be a bad request: %s", w.Body.String())
	}
}

// A body carrying nothing but an id is now the only shape there is.
func TestCreateAcceptsBodyWithoutAgent(t *testing.T) {
	_, h := newTestServer(t)
	w := postCreate(t, h, `{"id":"alice"}`)
	if w.Code == http.StatusBadRequest {
		t.Errorf("an id-only body must not be a bad request: %s", w.Body.String())
	}
}
