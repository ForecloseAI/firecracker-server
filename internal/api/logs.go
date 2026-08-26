package api

import (
	"context"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// units the dashboard may read. A whitelist rather than a free string: the name
// becomes a journalctl argument, and "whatever the caller sent" is how a log
// viewer turns into a way to read the whole system journal.
var units = map[string]bool{
	"cracked": true, "cracked-chat": true, "caddy": true,
}

// logLines bounds one read. Enough to see a session, short enough that the
// dashboard's poll stays cheap.
const (
	logLines   = 300
	logMaxWait = 4 * time.Second
)

// handleLogs tails one service's journal.
//
// The control plane reads these because it is what the dashboard is served from;
// the chat service's own journal is where every API request now lands, and that
// is the log an integration problem shows up in.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	unit := r.URL.Query().Get("unit")
	if !units[unit] {
		writeErr(w, http.StatusBadRequest, apiError{"bad_request", "unknown unit", ""})
		return
	}
	out, err := journal(unit, logCount(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, apiError{"internal", err.Error(), ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unit": unit, "lines": out})
}

// logCount is how many lines to return, bounded.
func logCount(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n < 1 || n > logLines {
		return logLines
	}
	return n
}

// journal runs journalctl for one unit.
//
// Reading another service's journal needs the systemd-journal group; without it
// this returns a permissions notice rather than lines, which is why the error is
// surfaced instead of being flattened to an empty log that looks like silence.
func journal(unit string, n int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), logMaxWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "-u", unit,
		"-n", strconv.Itoa(n), "--no-pager", "-o", "short-iso", "--output-fields=MESSAGE")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}
