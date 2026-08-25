package agentd

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// decodeBody unmarshals a JSON response body into out.
func decodeBody(t *testing.T, body string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

// /debug/exec is the only way into a guest running this daemon -- no vsock, no
// metadata service, no sshd -- so it must report the exit code and both streams
// separately, and must answer 200 even when the command itself failed.
func TestDebugExecReturnsExitCodeAndStreams(t *testing.T) {
	s := NewServer(newTestSupervisor(t))
	w := do(t, s, "POST", "/debug/exec", `{"cmd":"echo out; echo err >&2; exit 3"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even for a failed command", w.Code)
	}
	var got execResp
	decodeBody(t, w.Body.String(), &got)
	if got.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", got.ExitCode)
	}
	if !strings.Contains(got.Stdout, "out") || strings.Contains(got.Stdout, "err") {
		t.Errorf("stdout must hold only stdout, got %q", got.Stdout)
	}
	if !strings.Contains(got.Stderr, "err") {
		t.Errorf("stderr = %q, want it to carry the stderr line", got.Stderr)
	}
}

// An empty command is a client mistake, not a shell no-op, and mirrors the
// TypeScript agent so the dashboard sees one shape from either implementation.
func TestDebugExecRejectsEmptyCommand(t *testing.T) {
	s := NewServer(newTestSupervisor(t))
	if w := do(t, s, "POST", "/debug/exec", `{"cmd":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// The Bash tool must run bash, not sh. In the guest /bin/sh is dash, while on a
// developer's mac it is bash in POSIX mode -- so a bashism silently works in dev
// and fails only in production. This pins the shell by using syntax dash rejects.
func TestDebugExecUsesBash(t *testing.T) {
	s := NewServer(newTestSupervisor(t))
	w := do(t, s, "POST", "/debug/exec", `{"cmd":"[[ 1 == 1 ]] && echo bashism-ran"}`)
	var got execResp
	decodeBody(t, w.Body.String(), &got)
	if got.ExitCode != 0 || !strings.Contains(got.Stdout, "bashism-ran") {
		t.Errorf("bash-only syntax failed (exit %d, out %q, err %q): shell is not bash",
			got.ExitCode, got.Stdout, got.Stderr)
	}
}

// /health carries session_state so internal/agent.Health decodes it unchanged,
// and reading it must NOT start an agent: Get() starts one, and a dashboard
// polling every few seconds would otherwise pull the whole roster into memory.
func TestHealthReportsStateWithoutStartingAgents(t *testing.T) {
	sup := newTestSupervisor(t)
	s := NewServer(sup)
	before := sup.LiveCount()
	var got struct {
		OK           bool   `json:"ok"`
		SessionState string `json:"session_state"`
	}
	decodeBody(t, do(t, s, "GET", "/health", "").Body.String(), &got)
	if !got.OK || got.SessionState != "idle" {
		t.Errorf("health = %+v, want ok with session_state idle", got)
	}
	if sup.LiveCount() != before {
		t.Errorf("a health poll started %d agent(s)", sup.LiveCount()-before)
	}
}

// rss_bytes is how memory gets watched inside a guest, where the Go heap is only
// part of what the kernel holds. It degrades to 0 off Linux rather than erroring,
// so this asserts presence everywhere and a real value only where /proc exists.
func TestMemstatsReportsRSS(t *testing.T) {
	s := NewServer(newTestSupervisor(t))
	var got memReport
	decodeBody(t, do(t, s, "GET", "/debug/memstats", "").Body.String(), &got)
	if got.RSSBytes < 0 {
		t.Errorf("rss_bytes = %d, want >= 0", got.RSSBytes)
	}
	if runtime.GOOS == "linux" && got.RSSBytes == 0 {
		t.Error("rss_bytes should be non-zero on linux, where /proc exists")
	}
}
