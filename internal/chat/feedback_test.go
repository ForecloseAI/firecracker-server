package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSheet stands in for the deployed Apps Script web app.
type fakeSheet struct {
	mu   sync.Mutex
	rows []feedbackRow
	code int // non-zero to make the next append fail with this status
}

// routes answers the way the real script does: the row, then a small JSON body.
func (f *fakeSheet) routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var row feedbackRow
		json.NewDecoder(r.Body).Decode(&row)
		f.mu.Lock()
		f.rows = append(f.rows, row)
		code := f.code
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

// testEmail is the address on the access token every feedback test signs in
// with, and so the one the row must be stamped with.
const testEmail = "tester@example.com"

// serverWithSheet wires a gateway whose feedback reaches a fake webhook, and
// returns the access token of the person rating. control is deliberately left
// nil. Feedback must never reach the guest, so a handler that grew a guestOf
// call would panic here rather than quietly start costing a VM boot per star
// rating.
func serverWithSheet(t *testing.T, f *fakeSheet) (*Server, string) {
	t.Helper()
	srv := httptest.NewServer(f.routes())
	t.Cleanup(srv.Close)
	s, tok := signedIn(t)
	s.feedback = newSheet(srv.URL)
	return s, tok
}

// signedIn is a gateway with no guest behind it and a token that gets through
// its guard.
func signedIn(t *testing.T) (*Server, string) {
	t.Helper()
	v, mint := testAuth(t)
	s := &Server{auth: v, cfg: Config{Origin: "https://chat.example.com"}}
	return s, mint(testUserID, testEmail)
}

// The happy path, and the shape of a row: what the person typed, plus who and
// when, which they never sent.
func TestFeedbackRecordsARow(t *testing.T) {
	f := &fakeSheet{}
	s, u := serverWithSheet(t, f)

	w := call(t, s, u, "POST", "/v1/feedback",
		`{"agentId":"boss","rating":4,"comment":"nearly","taskTitle":"Book flights"}`)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if len(f.rows) != 1 {
		t.Fatalf("sheet got %d rows, want 1", len(f.rows))
	}
	row := f.rows[0]
	if row.Rating != 4 || row.Comment != "nearly" || row.TaskTitle != "Book flights" {
		t.Errorf("row = %+v", row)
	}
	if row.Email != testEmail || row.Machine != machineFor(testUserID) || row.Time == "" {
		t.Errorf("row was not stamped server-side: %+v", row)
	}
}

// The client cannot say who is rating. Otherwise anyone with a token could file
// feedback under a colleague's name.
func TestFeedbackIdentifiesTheTokenHolder(t *testing.T) {
	f := &fakeSheet{}
	s, u := serverWithSheet(t, f)

	call(t, s, u, "POST", "/v1/feedback",
		`{"agentId":"boss","rating":5,"email":"someone@else.com","time":"1999-01-01"}`)
	if len(f.rows) != 1 {
		t.Fatalf("sheet got %d rows, want 1", len(f.rows))
	}
	if f.rows[0].Email != testEmail {
		t.Errorf("email = %q, want the token holder %q", f.rows[0].Email, testEmail)
	}
	if strings.HasPrefix(f.rows[0].Time, "1999") {
		t.Errorf("the client set the time: %q", f.rows[0].Time)
	}
}

// A rating outside the five stars offered is a client bug, and storing it would
// quietly skew the only number this whole feature exists to collect.
func TestFeedbackRejectsWhatTheCardCannotProduce(t *testing.T) {
	for _, body := range []string{
		`{"agentId":"boss","rating":0}`,
		`{"agentId":"boss","rating":6}`,
		`{"agentId":"boss","rating":-1}`,
		`{"rating":3}`,
		`not json`,
	} {
		f := &fakeSheet{}
		s, u := serverWithSheet(t, f)
		w := call(t, s, u, "POST", "/v1/feedback", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400", body, w.Code)
		}
		if len(f.rows) != 0 {
			t.Errorf("%s reached the sheet", body)
		}
	}
}

// With no webhook configured there is nowhere to put this. Saying so is better
// than a 204 that means the person's answer went in the bin.
func TestFeedbackRefusesWhenNoSheetIsConfigured(t *testing.T) {
	s, u := signedIn(t)
	w := call(t, s, u, "POST", "/v1/feedback", `{"agentId":"boss","rating":5}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// A webhook that is down must not read as stored.
func TestFeedbackReportsAWebhookFailure(t *testing.T) {
	f := &fakeSheet{code: http.StatusInternalServerError}
	s, u := serverWithSheet(t, f)
	w := call(t, s, u, "POST", "/v1/feedback", `{"agentId":"boss","rating":5}`)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// Apps Script answers 200 even when the script threw, so the status alone
// cannot say whether the row landed. Believing it would report a lost rating as
// a thank-you.
func TestFeedbackBelievesTheBodyOverThe200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no such sheet"})
	}))
	t.Cleanup(srv.Close)
	s, u := signedIn(t)
	s.feedback = newSheet(srv.URL)

	w := call(t, s, u, "POST", "/v1/feedback", `{"agentId":"boss","rating":5}`)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// The endpoint is behind the same guard as everything else under /v1.
func TestFeedbackNeedsAToken(t *testing.T) {
	s, _, _ := newFake(t)
	r := httptest.NewRequest("POST", "/v1/feedback", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// Redaction of the comment is covered in logging_test.go, with the rest of the
// secrets list and through the middleware that actually prints it.
