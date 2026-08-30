package agentd

import (
	"context"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

type (
	scheduleInput struct {
		Name string `json:"name" jsonschema:"required,description=A short name for this standing job such as morning inbox sweep"`
		When string `json:"when" jsonschema:"required,description=One of - every 30m - daily at 09:00 - weekly on mon at 09:00 - once on 2026-09-02 at 11:00. Use the once form whenever the thing happens one time only at a known date and time. Never book a repeating job whose task text checks the date and does nothing on the other days. Times are read in the person's own timezone so write their local clock time and do not convert it. Nothing tighter than every 15m"`
		Task string `json:"task" jsonschema:"required,description=The message to send yourself when it fires - you will read it in this same conversation"`
	}
	cancelScheduleInput struct {
		ID string `json:"id" jsonschema:"required,description=Id of the schedule to cancel - use list_schedules to see them"`
	}
)

// noSupervisor is what the schedule tools answer when there is nowhere to keep
// a schedule, which only happens in a unit test.
const noSupervisor = "Scheduling is not available on this machine."

// scheduleTools let an agent put itself on a timer. Available to every agent
// through alwaysAllowed rather than named in six profile files.
//
// Built whether or not there is a supervisor, unlike teamTools. These names are
// in alwaysAllowed, and a tool that is always ALLOWED but only sometimes BUILT
// is precisely the silent drift the rest of this package guards against -- the
// surface would differ between configurations with nothing reporting it. Without
// a supervisor the handlers say so instead.
func scheduleTools(d toolDeps) ([]anthropic.BetaTool, error) {
	return buildTools(
		func() (anthropic.BetaTool, error) { return scheduleTaskTool(d) },
		func() (anthropic.BetaTool, error) { return listSchedulesTool(d) },
		func() (anthropic.BetaTool, error) { return cancelScheduleTool(d) },
	)
}

// scheduleTaskTool books a standing job, once the person has agreed to it.
//
// Gated, unlike the rest of the team tools. This is the one tool that commits
// the machine to spending money later with nobody watching, so the person agrees
// to it at the moment it is created rather than discovering it on a bill.
func scheduleTaskTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[scheduleInput](
		"schedule_task",
		"Arrange to message yourself at a set time: over and over, or just once on a given "+
			"date. The message arrives in this conversation, so write it as a note to "+
			"yourself. Needs the person's approval.",
		func(ctx context.Context, in scheduleInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if d.team == nil {
				return toolText(noSupervisor), nil
			}
			return toolText(d.team.CreateSchedule(ctx, d.gate, d.self, in)), nil
		})
}

// listSchedulesTool shows what this agent has standing.
func listSchedulesTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[noInput](
		"list_schedules", "List your standing scheduled jobs and when each next runs.",
		func(ctx context.Context, _ noInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if d.team == nil {
				return toolText(noSupervisor), nil
			}
			return toolText(renderSchedules(d.team.Schedules().List(), d.self)), nil
		})
}

// cancelScheduleTool stops a standing job. Not gated: undoing a commitment does
// not need the same permission as making one.
//
// Only the agent's own jobs, and a colleague's answers the same way an invented
// id does. list_schedules already filters by owner, so an id belonging to
// someone else can only have arrived out of band -- a colleague message, a
// localhost call -- and cancelling on it would undo work its owner is relying
// on. The route is deliberately not restricted this way: that request is the
// person, who owns every schedule on the machine.
func cancelScheduleTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[cancelScheduleInput](
		"cancel_schedule", "Stop one of your standing scheduled jobs.",
		func(ctx context.Context, in cancelScheduleInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if d.team == nil {
				return toolText(noSupervisor), nil
			}
			missing := toolText("There is no schedule " + in.ID + ". Use list_schedules.")
			if owner, ok := d.team.Schedules().Owner(in.ID); !ok || owner != d.self {
				return missing, nil
			}
			deleted, err := d.team.Schedules().Delete(in.ID)
			if err != nil {
				return toolText("Could not cancel " + in.ID + ": " + err.Error() +
					". It is still scheduled."), nil
			}
			if !deleted {
				return missing, nil
			}
			return toolText("Cancelled " + in.ID + "."), nil
		})
}

// CreateSchedule asks the person, then books the job. The whole handler lives
// here rather than in the tool so the approval and the write cannot drift apart.
func (s *Supervisor) CreateSchedule(ctx context.Context, gate *Gate, self string, in scheduleInput) string {
	loc := loadZone(s.stateDir)
	sp, at, err := planSchedule(in.When, loc, time.Now())
	if err != nil {
		return "That schedule is not valid: " + err.Error()
	}
	preview := in.Name + " - " + in.When + ": " + in.Task
	if err := gate.Check(ctx, "schedule_task", preview, in); err != nil {
		return err.Error() // the gate already words a refusal, including "do not retry"
	}
	sc, err := s.schedules.Add(Schedule{Name: in.Name, Agent: self, Task: in.Task,
		Expr: in.When, NextRunAt: at})
	if err != nil {
		return "Could not save that schedule: " + err.Error()
	}
	// Said differently for a one-off, so the model can see from the answer that
	// it booked a single run and does not go on to add a cancellation of its own.
	when := "First run "
	if sp.once {
		when = "Runs once at "
	}
	return "Scheduled as " + sc.ID + ". " + when + sc.NextRunAt.In(loc).Format(time.RFC1123) + "."
}

// renderSchedules lists one agent's schedules for the model to read.
func renderSchedules(all []Schedule, self string) string {
	var out []string
	for _, sc := range all {
		if sc.Agent != self {
			continue
		}
		line := sc.ID + " " + sc.Name + " (" + sc.Expr + ") next " + sc.NextRunAt.Format(time.RFC1123)
		if !sc.Enabled {
			line += " [off]"
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return "You have no scheduled jobs."
	}
	return strings.Join(out, "\n")
}
