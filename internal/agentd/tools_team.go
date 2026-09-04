package agentd

import (
	"context"
	"fmt"
	"strings"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// Inputs for the team tools. Descriptions must contain no commas: comma
// separates jsonschema tag options, so one would silently truncate the text
// the model sees.
type (
	startTaskInput struct {
		Title string `json:"title" jsonschema:"required,description=One line saying what this piece of work is"`
		Slug  string `json:"slug" jsonschema:"required,description=Two or three words for the folder name - lowercase with hyphens"`
	}
	messageAgentInput struct {
		To   string `json:"to" jsonschema:"required,description=Id of the agent to message - use list_agents to see them"`
		Text string `json:"text" jsonschema:"required,description=What you want to say"`
	}
	delegateInput struct {
		Agent   string `json:"agent" jsonschema:"required,description=Id of the agent to give this work to"`
		Title   string `json:"title" jsonschema:"required,description=One line naming the piece of work"`
		Task    string `json:"task" jsonschema:"required,description=Everything they need to do it - they cannot see your conversation"`
		TaskDir string `json:"task_dir" jsonschema:"description=Folder to work in - defaults to your current task folder"`
	}
	createAgentInput struct {
		Type string `json:"type" jsonschema:"required,description=Profile key from list_agent_types"`
		Name string `json:"name" jsonschema:"required,description=A human first name for them such as Maya or Tom - never a job description"`
	}
	deleteAgentInput struct {
		ID string `json:"id" jsonschema:"required,description=Id of the agent to remove"`
	}
	noInput struct{}
)

// teamTools are the tools that reach other agents. Which of them an agent
// actually gets is decided by its profile's tool list, so delegation is
// structurally boss-only: the tool is never sent to anyone else's model.
func teamTools(d toolDeps) ([]anthropic.BetaTool, error) {
	if d.team == nil {
		return nil, nil // an agent built without a supervisor works alone
	}
	return buildTools(
		func() (anthropic.BetaTool, error) { return startTaskTool(d) },
		func() (anthropic.BetaTool, error) { return listAgentsTool(d) },
		func() (anthropic.BetaTool, error) { return messageAgentTool(d) },
		func() (anthropic.BetaTool, error) { return delegateTool(d) },
		func() (anthropic.BetaTool, error) { return listTypesTool(d) },
		func() (anthropic.BetaTool, error) { return createAgentTool(d) },
		func() (anthropic.BetaTool, error) { return deleteAgentTool(d) },
	)
}

// startTaskTool opens a dated folder for a new piece of work.
func startTaskTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[startTaskInput](
		"start_task",
		"Open a folder for a new piece of work and make it what you are currently doing. "+
			"Call this when the person asks for something unrelated to what you were last doing.",
		func(ctx context.Context, in startTaskInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			task, err := d.team.StartTask(d.self, in.Title, in.Slug)
			if err != nil {
				return toolText(err.Error()), nil
			}
			return toolText("Working in " + task.Dir + ". Put this task's files there."), nil
		})
}

// listAgentsTool shows who else is on this machine and what they are doing.
func listAgentsTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[noInput](
		"list_agents", "List the other agents on this machine and what each is doing.",
		func(ctx context.Context, _ noInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return toolText(renderAgents(d.team.List(), d.self)), nil
		})
}

// renderAgents describes the roster in a line per agent.
func renderAgents(statuses []Status, self string) string {
	var lines []string
	for _, st := range statuses {
		if st.ID == self {
			continue
		}
		line := fmt.Sprintf("%s (%s, %s) - %s", st.ID, st.Name, st.Type, st.State)
		if st.Task != nil {
			line += ": " + st.Task.Title
		}
		lines = append(lines, line)
	}
	return joinOrNone(lines, "You are the only agent on this machine.")
}

// messageAgentTool sends a note to a colleague. Talking, not assigning.
func messageAgentTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[messageAgentInput](
		"message_agent",
		"Send a message to another agent. Use it to ask a question or report back. "+
			"It does not wait for a reply - theirs arrives as a message to you.",
		func(ctx context.Context, in messageAgentInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if err := d.team.Deliver(d.self, in.To, in.Text); err != nil {
				return toolText(err.Error()), nil
			}
			d.team.logFor(d.self, Event{Type: "agent_message", To: in.To, Text: in.Text})
			return toolText("Sent to " + in.To + "."), nil
		})
}

// delegateTool hands work to a specialist. Boss only, by profile.
func delegateTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[delegateInput](
		"delegate",
		"Give a piece of work to another agent. Returns at once - they work while you carry on - "+
			"and they message you when they are done. Include everything they need in the task.",
		func(ctx context.Context, in delegateInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			dir := in.TaskDir
			if dir == "" {
				if t := d.team.CurrentTask(d.self); t != nil {
					dir = t.Dir
				}
			}
			err := d.team.Delegate(d.self, Delegation{To: in.Agent, Title: in.Title, Task: in.Task, TaskDir: dir})
			if err != nil {
				return toolText(err.Error()), nil
			}
			return toolText(in.Agent + " has started on " + in.Title +
				". Carry on with your own part; they will message you when done."), nil
		})
}

// listTypesTool shows what kinds of specialist can be created.
func listTypesTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[noInput](
		"list_agent_types", "List the kinds of specialist you can create.",
		func(ctx context.Context, _ noInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			var lines []string
			for _, p := range hireable(d.team.Catalog()) {
				lines = append(lines, p.Key+" - "+p.Description)
			}
			return toolText(strings.Join(lines, "\n")), nil
		})
}

// hireable is the gallery an agent may hire from: every profile but the shell
// a person's own role goes into, which has no role of its own to offer.
func hireable(c *Catalog) []Profile {
	var out []Profile
	for _, p := range c.List() {
		if p.Key != agentapi.CustomType {
			out = append(out, p)
		}
	}
	return out
}

// createAgentTool adds a specialist to the roster.
func createAgentTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[createAgentInput](
		"create_agent",
		"Create a new specialist. It costs nothing until you give it work. "+
			"Give them a human first name: the person sees this name at the top of a "+
			"conversation and says it back, and \"whatsapp-researcher\" is not a name.",
		func(ctx context.Context, in createAgentInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return toolText(hire(d.team, in)), nil
		})
}

// hire adds a specialist for an agent, or says why not. A custom agent is
// refused: its role is something only the person writes.
func hire(team *Supervisor, in createAgentInput) string {
	if in.Type == agentapi.CustomType {
		return "Custom agents are made by the person in the app. Pick a profile from list_agent_types."
	}
	rec, err := team.Create(in.Type, in.Name)
	if err != nil {
		return err.Error()
	}
	return "Created " + rec.ID + " (" + rec.Type + "). Delegate to it by that id."
}

// deleteAgentTool removes a specialist, keeping its files.
func deleteAgentTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[deleteAgentInput](
		"delete_agent",
		"Remove an agent from the roster. Its memory and history are kept.",
		func(ctx context.Context, in deleteAgentInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if err := d.team.Delete(in.ID, false); err != nil {
				return toolText(err.Error()), nil
			}
			return toolText("Removed " + in.ID + "."), nil
		})
}
