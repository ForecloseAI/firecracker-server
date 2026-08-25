package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"time"
)

// execTimeout bounds one operator command. Matches the TypeScript agent's 30s,
// so the dashboard's shell behaves identically against either implementation.
const execTimeout = 30 * time.Second

// execReq is the body of POST /debug/exec.
type execReq struct {
	Cmd string `json:"cmd"`
}

// execResp mirrors the TypeScript agent's shape exactly, so the dashboard's
// guest shell needs no change to work against a Go VM.
type execResp struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// handleDebugExec runs a shell command in the guest.
//
// This is an operator surface, not an agent tool: it bypasses the approval Gate
// deliberately, exactly as the TypeScript agent's /debug/exec does, and sits
// behind the control plane's authenticated /vms/{id}/agent/ proxy. Inside a
// microVM it is the ONLY way in -- there is no vsock, no metadata service and no
// sshd -- so without it a Go VM that misbehaves is a black box.
func (s *Server) handleDebugExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Cmd == "" {
		fail(w, http.StatusBadRequest, "bad_request", "cmd is required", "")
		return
	}
	reply(w, http.StatusOK, runDebug(r.Context(), req.Cmd))
}

// runDebug executes one command and reports how it went.
//
// A non-zero exit is a 200, not an HTTP error: the caller asked what happened,
// and "the command failed" is the answer, not a failure to answer.
func runDebug(ctx context.Context, command string) execResp {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Same reasoning as the Bash tool: a backgrounded grandchild holding the
	// pipe would otherwise block Run past both the timeout and the cancel.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return execResp{
		ExitCode: exitCodeOf(err),
		Stdout:   capTextAt(stdout.String(), bashOutputCap),
		Stderr:   capTextAt(stderr.String(), bashOutputCap),
	}
}

// exitCodeOf extracts a process exit status, using -1 for a command that never
// ran or was killed, which no real exit status can collide with.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
