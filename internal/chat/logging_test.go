package chat

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture runs one request through the middleware and returns what was logged.
func capture(t *testing.T, bodies bool, h http.HandlerFunc, r *http.Request) string {
	t.Helper()
	v, _ := testAuth(t)
	return captureAs(t, bodies, v, h, r)
}

// captureAs is capture against a specific verifier, for the one test that needs
// a token the middleware will actually accept.
func captureAs(t *testing.T, bodies bool, v *Verifier, h http.HandlerFunc, r *http.Request) string {
	t.Helper()
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(old) })
	logged(h, bodies, v).ServeHTTP(httptest.NewRecorder(), r)
	return out.String()
}

// The stream carries its token in the query string, because SSE cannot set
// headers. Logging it verbatim would leave a working credential in the journal.
func TestTokenIsRedactedFromTheURL(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/stream?token=tok_secret_value", nil)
	got := capture(t, true, func(w http.ResponseWriter, r *http.Request) {}, r)
	if strings.Contains(got, "tok_secret_value") {
		t.Fatalf("the token reached the log: %s", got)
	}
	if !strings.Contains(got, "token=REDACTED") {
		t.Errorf("log = %s", got)
	}
}

// A credential in either body must not reach the journal. There is no sign-in
// route here -- identity lives in Supabase -- so this is the backstop for a
// client that posts one anyway, and for anything that answers with a token.
func TestCredentialsAreRedactedBothWays(t *testing.T) {
	body := strings.NewReader(`{"email":"a@b.com","password":"hunter2"}`)
	r := httptest.NewRequest("POST", "/v1/profile", body)
	got := capture(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"userId":"a@b.com","token":"tok_live_credential"}`))
	}, r)
	for _, secret := range []string{"hunter2", "tok_live_credential"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q reached the log: %s", secret, got)
		}
	}
	if !strings.Contains(got, "a@b.com") {
		t.Error("redaction ate the whole line")
	}
}

// The handler must still receive the body the middleware read for logging.
func TestBodyStillReachesTheHandler(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/threads/coder/messages", strings.NewReader(`{"text":"ship it"}`))
	seen := ""
	capture(t, true, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		seen = string(buf[:n])
	}, r)
	if !strings.Contains(seen, "ship it") {
		t.Fatalf("handler received %q; the log middleware ate the body", seen)
	}
}

// One line per request, carrying what an integrator needs: outcome, route,
// timing, who, and both bodies.
func TestRequestLineHasWhatIsNeeded(t *testing.T) {
	v, mint := testAuth(t)
	r := httptest.NewRequest("POST", "/v1/threads/coder/messages", strings.NewReader(`{"text":"hi"}`))
	r.Header.Set("Authorization", "Bearer "+mint(testUserID, "tester@example.com"))
	got := captureAs(t, true, v, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"12"}`))
	}, r)
	for _, want := range []string{"201", "POST", "/v1/threads/coder/messages",
		"user=tester@example.com", `req={"text":"hi"}`, `res={"id":"12"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("log is missing %q:\n%s", want, got)
		}
	}
}

// An unauthenticated call is still logged, or a 401 loop would be invisible.
func TestAnonymousCallsAreLogged(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	got := capture(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, r)
	if !strings.Contains(got, "401") || !strings.Contains(got, "user=-") {
		t.Errorf("log = %s", got)
	}
}

// A stream stays open for as long as the app is running, so announcing it only
// when it ends would leave it invisible for the whole time it mattered.
func TestStreamLogsOpenAndClose(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/stream?token=abc", nil)
	got := capture(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"typing\"}\n\n"))
	}, r)
	if !strings.Contains(got, "SSE open") || !strings.Contains(got, "SSE close") {
		t.Fatalf("log = %s", got)
	}
	// Its body is endless; buffering it would grow without bound.
	if strings.Contains(got, "res=") {
		t.Errorf("the stream body was captured: %s", got)
	}
}

// Turning bodies off must leave the request line intact -- the switch is for
// privacy, not for going blind.
func TestBodiesCanBeTurnedOff(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/threads/coder/messages", strings.NewReader(`{"text":"private"}`))
	got := capture(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"1"}`))
	}, r)
	if strings.Contains(got, "private") || strings.Contains(got, "res=") {
		t.Errorf("bodies were logged with the switch off: %s", got)
	}
	if !strings.Contains(got, "POST") || !strings.Contains(got, "/v1/threads/coder/messages") {
		t.Errorf("the request line went missing too: %s", got)
	}
}

// A long body must not bury the next line.
func TestLongBodiesAreTrimmed(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	got := capture(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"x":"` + strings.Repeat("y", 5000) + `"}`))
	}, r)
	if len(got) > 1200 {
		t.Errorf("one line was %d bytes", len(got))
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("a body broke the line into %d:\n%s", strings.Count(got, "\n"), got)
	}
}
