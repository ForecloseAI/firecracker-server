package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"cracked/internal/vm"
)

// createReq is the POST /vms body. Sizing is fixed in v1, so id is the only field.
type createReq struct {
	ID string `json:"id"`
}

// handleHealth reports liveness and slot usage without requiring a token.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	c := s.reg.Capacity()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "firecracker_version": firecrackerVersion(),
		"slots_used": c.SlotsUsed, "slots_total": c.SlotsTotal,
	})
}

// firecrackerVersion shells out once per call; cheap and avoids caching stale info.
func firecrackerVersion() string {
	out, err := exec.Command("firecracker", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// handleCapacity reports the current resource allocation.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reg.Capacity())
}

// handleCreate boots a VM, blocking until its guest agent answers.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, apiError{"bad_request", "invalid JSON body", ""})
		return
	}
	if req.ID == "" {
		req.ID = generateID()
	}
	if !vm.ValidID(req.ID) {
		writeErr(w, http.StatusBadRequest,
			apiError{"bad_request", "id must match ^[a-z0-9][a-z0-9-]{0,31}$", ""})
		return
	}
	s.create(w, req.ID)
}

// create runs the boot and maps its failure modes onto status codes.
func (s *Server) create(w http.ResponseWriter, id string) {
	v, err := s.reg.Create(id)
	if err == nil {
		writeJSON(w, http.StatusCreated, s.reg.View(v))
		return
	}
	if isBootTimeout(err) {
		writeErr(w, http.StatusGatewayTimeout, apiError{"agent_timeout", err.Error(), ""})
		return
	}
	writeVMErr(w, err)
}

// isBootTimeout distinguishes "guest never came up" from a hard boot failure.
func isBootTimeout(err error) bool {
	return strings.Contains(err.Error(), "did not come up") ||
		strings.Contains(err.Error(), "exited during boot")
}

// handleList returns every live VM.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	vms := s.reg.List()
	views := make([]vm.View, 0, len(vms))
	for _, v := range vms {
		s.reg.Refresh(v)
		views = append(views, s.reg.View(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": views})
}

// handleGet returns one VM with its state refreshed from firecracker.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.reg.Get(r.PathValue("id"))
	if err != nil {
		writeVMErr(w, err)
		return
	}
	s.reg.Refresh(v)
	writeJSON(w, http.StatusOK, s.reg.View(v))
}

// handlePause freezes a running VM.
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, s.reg.Pause)
}

// handleResume unfreezes a paused VM.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, s.reg.Resume)
}

// transition applies a state change and renders the updated VM.
func (s *Server) transition(w http.ResponseWriter, r *http.Request, fn func(*vm.VM) error) {
	v, err := s.reg.Get(r.PathValue("id"))
	if err != nil {
		writeVMErr(w, err)
		return
	}
	if err := fn(v); err != nil {
		if errors.Is(err, vm.ErrState) {
			writeVMErr(w, err)
			return
		}
		writeErr(w, http.StatusBadGateway, apiError{"vm_unreachable", err.Error(), ""})
		return
	}
	writeJSON(w, http.StatusOK, s.reg.View(v))
}

// handleDelete stops a VM, purging the workspace only when asked.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	v, err := s.reg.Get(r.PathValue("id"))
	if err != nil {
		writeVMErr(w, err)
		return
	}
	purge := r.URL.Query().Get("purge") == "true"
	if err := s.reg.Delete(v, purge); err != nil {
		writeVMErr(w, err)
		return
	}
	// Drop the usage watermark too: a ?purge=true recreate resets the guest's
	// event log to id 1, and a stale high watermark would skip it entirely.
	s.usage.Forget(v.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": v.ID, "purged": purge})
}

// generateID builds a unique id when the caller supplies none. The random
// suffix matters: the sequence restarts at 1 on every server restart, but
// workspaces persist, so a bare counter could silently adopt a stale disk.
func generateID() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("vm-%d", nextSeq())
	}
	return fmt.Sprintf("vm-%d-%s", nextSeq(), hex.EncodeToString(b[:]))
}
