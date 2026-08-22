package fc

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// serveSock runs an HTTP server on a unix socket and reports how many
// connections are currently open. The directory is deliberately NOT t.TempDir:
// that embeds the test name, and a unix socket path is capped at ~104 bytes.
func serveSock(t *testing.T, h http.HandlerFunc) (path string, open *atomic.Int64) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	open = &atomic.Int64{}
	srv := &http.Server{Handler: h, ConnState: func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); os.RemoveAll(dir) })
	return path, open
}

// describeOK answers Describe with a healthy running VM.
func describeOK(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, `{"id":"probe","state":"Running","vmm_version":"1.16.1"}`)
}

// Firecracker allows only 10 concurrent API connections, and callers build a
// Client per operation. With keep-alives on, each call parks its connection in
// that throwaway Transport's idle pool, closed only when GC gets round to it,
// so anything polling faster than GC reclaims wedges the socket at "Too many
// open connections" -- taking Pause, Resume and graceful teardown with it.
// Every request must leave nothing behind.
func TestDescribeLeavesNoIdleConnection(t *testing.T) {
	path, open := serveSock(t, describeOK)
	for i := 0; i < 30; i++ { // well past firecracker's cap of 10
		if _, err := New(path).Describe(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := open.Load(); n > 1 {
		t.Errorf("%d connections still open after 30 sequential calls; "+
			"firecracker caps at 10, so these must not be pooled", n)
	}
}

func TestDescribe(t *testing.T) {
	path, _ := serveSock(t, describeOK)
	info, err := New(path).Describe()
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "Running" || info.VMMVersion != "1.16.1" {
		t.Errorf("info = %+v", info)
	}
}

func TestActionAndSetVMState(t *testing.T) {
	var got []string
	path, _ := serveSock(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(buf[:n]))
	})
	if err := New(path).Action("SendCtrlAltDel"); err != nil {
		t.Fatal(err)
	}
	if err := New(path).SetVMState("Paused"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PUT /actions {"action_type":"SendCtrlAltDel"}`,
		`PATCH /vm {"state":"Paused"}`,
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A non-2xx must surface as an error carrying the body, not decode into a
// zero-valued success. That is exactly how "Too many open connections" showed
// up as nothing more than a bare "?" in the dashboard.
func TestErrorResponseCarriesBody(t *testing.T) {
	path, _ := serveSock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{ "error": "Too many open connections" }`)
	})
	_, err := New(path).Describe()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Too many open connections") {
		t.Errorf("error = %q, want the firecracker message in it", err)
	}
}
