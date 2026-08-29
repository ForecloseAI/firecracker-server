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
// ClientTime and TZ ride along here as well as on a message, because the first
// thing a new client may do is create a schedule, and "daily at 09:00" needs to
// know whose 9am before it is stored.
type scheduleReq struct {
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	Task       string `json:"task"`
	Expr       string `json:"expr"`
	ClientTime string `json:"client_time,omitempty"`
	TZ         string `json:"tz,omitempty"`
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
	RememberZone(s.sup.stateDir, req.TZ, req.ClientTime)
	if req.Name == "" || req.Task == "" || req.Expr == "" {
		fail(w, http.StatusBadRequest, "bad_request", "name, task and expr are required", "")
		return
	}
	if _, ok := s.sup.Roster().Get(orDefault(req.Agent, BossID)); !ok {
		fail(w, http.StatusNotFound, "not_found", "no such agent", "agent")
		return
	}
	sp, err := parseSchedule(req.Expr, loadZone(s.sup.stateDir))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error(), "schedule")
		return
	}
	sc, err := s.sup.Schedules().Add(Schedule{
		Name: req.Name, Agent: orDefault(req.Agent, BossID), Task: req.Task,
		Expr: req.Expr, NextRunAt: sp.next(time.Now()),
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "schedule")
		return
	}
	reply(w, http.StatusCreated, sc)
}

// handleDeleteSchedule cancels a schedule.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.sup.Schedules().Delete(id) {
		fail(w, http.StatusNotFound, "not_found", "no such schedule", "schedule")
		return
	}
	reply(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}
