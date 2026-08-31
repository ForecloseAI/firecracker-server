package agentd

import (
	"net/http"
	"time"
)

// scheduleBodyCap bounds a schedule upload. A task is a sentence or two;
// anything near this is a client bug.
const scheduleBodyCap = 32 << 10

// scheduleReq is the body of POST /schedules.
//
// "daily at 09:00" still needs to know whose 9am, but it no longer has to be
// told: loadZone below reads the zone onboarding stored on this machine.
type scheduleReq struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
	Task  string `json:"task"`
	Expr  string `json:"expr"`
}

// handleListSchedules reports every schedule on this machine.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.sup.Schedules().List())
}

// handleCreateSchedule books a schedule for an agent.
//
// Not gated, unlike the schedule_task tool: this request IS the person, so
// asking them to approve their own click would be theatre. The tool is gated
// because there a model is the one proposing it.
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleReq
	if !decode(w, r, scheduleBodyCap, &req) {
		return
	}
	if req.Name == "" || req.Task == "" || req.Expr == "" {
		fail(w, http.StatusBadRequest, "bad_request", "name, task and expr are required", "")
		return
	}
	if _, ok := s.sup.Roster().Get(orDefault(req.Agent, BossID)); !ok {
		fail(w, http.StatusNotFound, "not_found", "no such agent", "agent")
		return
	}
	// The stored expression is what planSchedule hands back rather than what was
	// posted: a relative one-off is resolved to the instant it named, so it does
	// not mean something different on every sweep.
	_, expr, at, err := planSchedule(req.Expr, loadZone(s.sup.stateDir), time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error(), "schedule")
		return
	}
	sc, err := s.sup.Schedules().Add(Schedule{
		Name: req.Name, Agent: orDefault(req.Agent, BossID), Task: req.Task,
		Expr: expr, NextRunAt: at,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "schedule")
		return
	}
	reply(w, http.StatusCreated, sc)
}

// handleDeleteSchedule cancels a schedule.
//
// A write that did not land is a 500 and not a 200, for the same reason Add's
// is: the schedule is still on the disk and comes back at the next restart, so
// a client told "deleted" would stop showing something that still fires.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deleted, err := s.sup.Schedules().Delete(id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "schedule")
		return
	}
	if !deleted {
		fail(w, http.StatusNotFound, "not_found", "no such schedule", "schedule")
		return
	}
	reply(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}
