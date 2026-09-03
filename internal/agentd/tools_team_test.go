package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// toolNames lists what an agent's model can actually see.
func toolNames(a *Agent) map[string]bool {
	out := map[string]bool{}
	for _, tool := range a.tools {
		out[tool.Name()] = true
	}
	return out
}

// Boss-only is structural, not advice. A worker's model never receives the
// delegate tool, so no prompt injection or clever argument can reach it -- the
// tool is simply not in the request.
func TestOnlyTheBossCanDelegate(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")

	boss, err := sup.Get(BossID)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := sup.Get("ada")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"delegate", "create_agent", "delete_agent", "list_agent_types"} {
		if !toolNames(boss)[name] {
			t.Errorf("the boss is missing %s", name)
		}
		if toolNames(worker)[name] {
			t.Errorf("a worker was given %s", name)
		}
	}
	// Talking is for everyone; assigning and opening task folders are not.
	for _, name := range []string{"message_agent", "list_agents"} {
		if !toolNames(worker)[name] {
			t.Errorf("a worker is missing %s", name)
		}
	}
}

// A message must land in the recipient's inbox and in both logs, because each
// per-agent log is the only transcript of that agent.
func TestPeerMessageIsRecordedInBothLogs(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")
	boss, _ := sup.Get(BossID)
	ada, _ := sup.Get("ada")

	if err := sup.Deliver(BossID, "ada", "please look at the parser"); err != nil {
		t.Fatal(err)
	}
	sup.logFor(BossID, Event{Type: "agent_message", To: "ada", Text: "please look at the parser"})

	if !hasEvent(t, ada, "agent_message", "boss") {
		t.Error("the recipient's log has no incoming message")
	}
	if !hasEvent(t, boss, "agent_message", "ada") {
		t.Error("the sender's log has no outgoing message")
	}
}

// hasEvent reports whether a log holds an event of this type naming who.
func hasEvent(t *testing.T, a *Agent, kind, who string) bool {
	t.Helper()
	events, err := a.Log().ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == kind && (e.From == who || e.To == who) {
			return true
		}
	}
	return false
}

// A message from an agent must be labelled as such. The limits say colleagues
// are not a chain of command, and that rule only works if the model can tell
// a colleague's message from the person's.
func TestAgentMessagesAreLabelledNotDisguisedAsThePerson(t *testing.T) {
	fromPerson := frame(inbound{text: "do the thing"})
	if fromPerson != "do the thing" {
		t.Errorf("the person's message was reframed: %q", fromPerson)
	}
	fromAgent := frame(inbound{text: "do the thing", from: "boss"})
	if !strings.Contains(fromAgent, "boss") || !strings.Contains(fromAgent, "colleague") {
		t.Errorf("an agent's message was not labelled: %q", fromAgent)
	}
}

// Delivery must start an evicted agent, or work would stall depending on what
// happened to be in memory when a colleague reached for it.
func TestDeliveryStartsAnEvictedAgent(t *testing.T) {
	sup := supervisorWith(t, 1)
	sup.Create("coder", "Ada")
	sup.Get(BossID) // occupies the only slot

	if err := sup.Deliver(BossID, "ada", "wake up"); err != nil {
		t.Fatalf("delivery to a stopped agent failed: %v", err)
	}
}

// Delegating to somebody who does not exist should say so usefully rather than
// failing silently and leaving the boss thinking work is under way.
func TestDelegateToAnUnknownAgentSaysSo(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Get(BossID)
	err := sup.Delegate(BossID, Delegation{To: "nobody", Title: "t", Task: "x"})
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("delegate to a missing agent = %v, want it named", err)
	}
}

// The brief has to be self-contained: a worker cannot see the boss's
// conversation, so anything left out is simply lost.
func TestDelegationBriefCarriesEverything(t *testing.T) {
	brief := delegationBrief("boss", Delegation{
		Title: "write the parser", Task: "handle quoted commas", TaskDir: "/w/2026-08-25-csv",
	})
	for _, want := range []string{"write the parser", "handle quoted commas", "/w/2026-08-25-csv", "message_agent", "boss"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief is missing %q:\n%s", want, brief)
		}
	}
}

// A task folder is dated so a workspace stays navigable over time, and the
// roster records it so a poll can say what an agent is doing.
func TestStartTaskMakesADatedFolderAndRecordsIt(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Get(BossID)

	task, err := sup.StartTask(BossID, "Parse the CSV export", "csv-export")
	if err != nil {
		t.Fatal(err)
	}
	// The machine's clock, not the test process's: StartTask dates the folder
	// from the stored zone, and computing the expectation from time.Local made
	// this pass or fail on where the host happened to be.
	want := sup.localDate(time.Now()) + "-csv-export"
	if filepath.Base(task.Dir) != want {
		t.Errorf("folder = %q, want %q", filepath.Base(task.Dir), want)
	}
	if _, err := os.Stat(task.Dir); err != nil {
		t.Errorf("the folder was not created: %v", err)
	}
	// Both halves of what the roster records: delegate reads Dir back off it to
	// keep the pieces of one job together instead of each agent opening its own.
	if got := sup.CurrentTask(BossID); got == nil || got.Title != "Parse the CSV export" ||
		got.Dir != task.Dir {
		t.Errorf("roster task = %+v, want the title and folder recorded", got)
	}
}

// An agent messaging itself is a loop, not collaboration.
func TestAnAgentCannotMessageItself(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Get(BossID)
	if err := sup.Deliver(BossID, BossID, "hello me"); err == nil {
		t.Error("an agent messaged itself")
	}
}

// errStub stands in for any turn failure.
type errStub string

func (e errStub) Error() string { return string(e) }

// A worker that stops without reporting leaves its delegator waiting forever.
// The first end-to-end run did exactly that: a coder ran out of steps mid-task,
// never messaged back, and a second agent stood by for a message that was never
// coming. A turn that ended is news whether or not it went well.
func TestDelegatedWorkIsReportedEvenWhenItFails(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")
	boss, _ := sup.Get(BossID)
	ada, _ := sup.Get("ada")

	ada.turnStartID = ada.Log().LastID()
	ada.report(inbound{replyTo: BossID}, nil, errStub("the turn failed"))

	if !hasEvent(t, boss, "agent_message", "ada") {
		t.Error("a failed delegation was never reported back to the boss")
	}
}

// An agent that already messaged its delegator must not be made to say it
// twice: the model's own report is better than a generated one.
func TestNoDuplicateReportWhenTheAgentAlreadyMessaged(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")
	sup.Get(BossID)
	ada, _ := sup.Get("ada")

	ada.turnStartID = ada.Log().LastID()
	ada.Log().Append(Event{Type: "agent_message", To: BossID, Text: "done, parser.go is written"})
	if !ada.messagedDuring(BossID, ada.turnStartID) {
		t.Fatal("an outgoing message during the turn was not seen")
	}
}

// Only the boss opens task folders. A worker handed a folder that opens its own
// defeats the point, which is that one job's pieces end up together.
func TestWorkersCannotOpenTaskFolders(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")
	boss, _ := sup.Get(BossID)
	ada, _ := sup.Get("ada")

	if !toolNames(boss)["start_task"] {
		t.Error("the boss cannot open a task folder")
	}
	if toolNames(ada)["start_task"] {
		t.Error("a worker was given start_task")
	}
}
