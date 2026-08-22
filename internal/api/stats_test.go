package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cracked/internal/vm"
)

// newTestServer builds a server over an empty registry rooted in a temp dir.
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := New(vm.NewRegistry(t.TempDir(), "cracked"), "s3cret")
	return s, s.Routes()
}

// The dashboard must never set a cookie. One would be scoped to "/", and the
// untrusted guest serves same-origin content under /vms/{id}/agent/, so an
// injected page there could ride it and drive the whole fleet.
func TestDashboardSetsNoCookie(t *testing.T) {
	_, h := newTestServer(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/dashboard?token=s3cret", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if c := w.Result().Cookies(); len(c) != 0 {
		t.Errorf("dashboard set %d cookie(s): %v", len(c), c)
	}
	if !strings.Contains(w.Body.String(), "<title>cracked</title>") {
		t.Error("dashboard body is not the embedded page")
	}
}

// The VNC subtree still needs the cookie, because browsers cannot set headers
// on subresource loads or WebSocket handshakes. It must stay scoped to that one
// VM's path, which is what keeps a compromised guest from reaching the fleet.
func TestVNCQueryTokenSetsPathScopedCookie(t *testing.T) {
	_, h := newTestServer(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/vms/alice/vnc/vnc.html?token=s3cret", nil))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want exactly one cookie, got %v", cookies)
	}
	if cookies[0].Path != "/vms/alice/" {
		t.Errorf("cookie path = %q, want /vms/alice/", cookies[0].Path)
	}
}

// None of the dashboard's own routes may set a cookie: they have no {id}, so
// theirs would be scoped to "/", and the untrusted guest serves same-origin
// content under /vms/{id}/agent/ where an injected page could ride it.
func TestDashboardRoutesSetNoCookie(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/stats", "/metrics", "/vms/alice/stats"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path+"?token=s3cret", nil))
		if c := w.Result().Cookies(); len(c) != 0 {
			t.Errorf("%s set %d cookie(s): %v", path, len(c), c)
		}
	}
}

func TestStatsRequiresToken(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/stats", "/metrics", "/dashboard"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated = %d, want 401", path, w.Code)
		}
	}
}

// A bare visit should land on the dashboard rather than a 404 or a raw 401.
func TestRootRedirectsPreservingToken(t *testing.T) {
	_, h := newTestServer(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/?token=s3cret", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/dashboard?token=s3cret" {
		t.Errorf("Location = %q", got)
	}
}

// An empty host must still render a well-formed exposition, and each metric
// family's HELP must appear exactly once or strict parsers reject the scrape.
func TestMetricsExposition(t *testing.T) {
	_, h := newTestServer(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, name := range []string{"cracked_slots_total", "cracked_vm_up", "cracked_vm_agent_tokens_total"} {
		if n := strings.Count(body, "# HELP "+name+" "); n != 1 {
			t.Errorf("HELP for %s appears %d times, want 1", name, n)
		}
	}
	if !strings.Contains(body, "# TYPE cracked_vm_cpu_seconds_total counter") {
		t.Error("cpu must be a counter so rate() can be applied to it")
	}
}

// With a VM allocated but not running, every family gains a labelled sample
// and the guest probe degrades instead of failing the scrape.
func TestMetricsLabelsEachVM(t *testing.T) {
	s, h := newTestServer(t)
	if _, err := s.reg.Allocate("probe"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `cracked_vm_up{vm="probe"} 0`) {
		t.Errorf("missing or non-zero up sample for a creating VM:\n%s", body)
	}
	if !strings.Contains(body, `cracked_vm_agent_tokens_total{vm="probe",kind="cache_read"} 0`) {
		t.Error("token family is missing its kind split")
	}
}

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailFile(path, 4); got != "6789" {
		t.Errorf("tail(4) = %q, want %q", got, "6789")
	}
	if got := tailFile(path, 100); got != "0123456789" {
		t.Errorf("tail longer than the file = %q, want the whole file", got)
	}
	if got := tailFile(filepath.Join(t.TempDir(), "gone.log"), 10); got != "" {
		t.Errorf("missing file = %q, want empty", got)
	}
}
