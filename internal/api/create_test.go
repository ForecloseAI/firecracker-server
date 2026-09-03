package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A caller still sending the retired `agent` field must not break, and an
// id-only body -- now the only shape there is -- must not either. Nothing in
// the guest can act on `agent` any more, and rejecting it would turn a harmless
// stale client into a hard failure for no gain: encoding/json drops unknown
// fields, and this pins that it stays that way.
func TestCreateAcceptsAnIdOnlyBodyAndIgnoresTheRetiredAgentField(t *testing.T) {
	_, h := newTestServer(t)
	for _, body := range []string{`{"id":"alice","agent":"go"}`, `{"id":"alice"}`} {
		r := httptest.NewRequest("POST", "/vms", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer s3cret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		// Booting fails in a temp dir with no kernel or rootfs, but it must fail
		// LATER than validation: a 400 here would mean the field became meaningful.
		if w.Code == http.StatusBadRequest {
			t.Errorf("body %s must not be a bad request: %s", body, w.Body.String())
		}
	}
}
