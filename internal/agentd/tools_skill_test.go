package agentd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// skillTools builds the tool surface for an agent rooted at agentDir.
func skillTools(t *testing.T, builtin, agentDir string) ([]anthropic.BetaTool, *reloadFlag) {
	t.Helper()
	reload := &reloadFlag{}
	r := roots{workspace: t.TempDir(), own: agentDir, builtin: builtin}
	tools, err := Tools(r, toolDeps{
		gate:   NewGate(mustLog(t), NewInteractions(), t.TempDir()),
		reload: reload,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools, reload
}

// THE one that matters. create_skill composes the front matter itself, so the
// property to hold is that whatever it writes, the loader reads back. A skill
// the loader rejects is invisible with no error anywhere.
func TestCreateSkillRoundTripsThroughTheLoader(t *testing.T) {
	builtin := t.TempDir()
	agentDir := t.TempDir()
	tools, reload := skillTools(t, builtin, agentDir)

	got := call(t, tools, "create_skill", createSkillInput{
		Name:        "Expense Filing",
		Description: "File an expense.\nUse when the person sends a receipt.",
		Body:        "1. Read the receipt\n2. File it",
	})
	if !strings.Contains(got, "expense-filing") {
		t.Fatalf("result = %q, want the normalised name", got)
	}

	skills, problems := LoadSkills(builtin, agentDir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want the written skill to parse cleanly", problems)
	}
	if len(skills) != 1 || skills[0].Name != "expense-filing" {
		t.Fatalf("skills = %+v, want the one just written", skills)
	}
	// The newline has to be gone: the front matter parser reads one line per
	// key, so a description spanning two would be silently truncated at the cut.
	if strings.Contains(skills[0].Description, "\n") || !strings.Contains(skills[0].Description, "receipt") {
		t.Errorf("description = %q, want it flattened onto one line and whole", skills[0].Description)
	}
	if !reload.take() {
		t.Error("the reload flag was not set, so the agent would never pick the skill up")
	}
}

// A refusal has to say what was wrong. The model's next move is to try again,
// and it can only fix what it was told.
func TestCreateSkillRefusalsExplainThemselves(t *testing.T) {
	builtin := t.TempDir()
	writeSkill(t, builtin, "pdf", "description: the shipped one")
	agentDir := t.TempDir()
	tools, reload := skillTools(t, builtin, agentDir)

	cases := map[string]struct {
		in   createSkillInput
		want string
	}{
		"no usable name":  {createSkillInput{Name: "!!!", Description: "d", Body: "b"}, "expense-filing"},
		"no description":  {createSkillInput{Name: "thing", Description: " ", Body: "b"}, "description"},
		"no body":         {createSkillInput{Name: "thing", Description: "d", Body: ""}, "steps"},
		"shadows builtin": {createSkillInput{Name: "pdf", Description: "d", Body: "b"}, "built-in"},
	}
	for name, tc := range cases {
		got := call(t, tools, "create_skill", tc.in)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: result = %q, want it to mention %q", name, got, tc.want)
		}
	}
	if reload.take() {
		t.Error("a refused skill still asked for a reload, which would recycle the agent for nothing")
	}
	if skills, _ := LoadSkills(builtin, agentDir); len(skills) != 1 || skills[0].Description != "the shipped one" {
		t.Errorf("skills = %+v, want the built-in untouched and nothing written", skills)
	}
}

// Every profile gets create_skill whether or not it names it. An agent that
// cannot record what it worked out has to rediscover it every time.
func TestCreateSkillIsAlwaysAllowed(t *testing.T) {
	tools, _ := skillTools(t, t.TempDir(), t.TempDir())
	narrow, err := Tools(roots{workspace: t.TempDir()},
		toolDeps{gate: NewGate(mustLog(t), NewInteractions(), t.TempDir())}, []string{"Read"})
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range [][]anthropic.BetaTool{tools, narrow} {
		if !hasTool(set, "create_skill") {
			t.Error("create_skill is missing from a tool set that should always carry it")
		}
	}
}

// A built-in is readable and not writable, and the refusal has to say so. The
// generic "outside the workspace" message would contradict the read the model
// just did and read as a bug rather than a rule.
func TestBuiltinSkillsAreReadableButNotWritable(t *testing.T) {
	builtin := t.TempDir()
	writeSkill(t, builtin, "pdf", "description: the shipped one")
	path := filepath.Join(builtin, "pdf", "SKILL.md")
	tools, _ := skillTools(t, builtin, t.TempDir())

	if got := call(t, tools, "Read", readInput{Path: path}); !strings.Contains(got, "steps go here") {
		t.Errorf("Read = %q, want the built-in's contents", got)
	}
	if got := call(t, tools, "Write", writeInput{Path: path, Content: "mine now"}); !strings.Contains(got, "read-only") {
		t.Errorf("Write = %q, want a refusal naming the rule", got)
	}
	if got := call(t, tools, "Edit", editInput{Path: path, OldString: "steps", NewString: "no"}); !strings.Contains(got, "read-only") {
		t.Errorf("Edit = %q, want a refusal naming the rule", got)
	}
}

// THE end-to-end of the feature inside Go: writing a skill during a turn makes
// the agent ask to be recycled once the turn is over, which is what puts the
// new skill in its prompt without a rebuild or a restart of anything.
func TestSkillWrittenDuringATurnRecyclesTheAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	a, stopped := liveWithoutGoroutine(t, sup, "helper")

	call(t, a.tools, "create_skill", createSkillInput{
		Name: "expense-filing", Description: "File an expense.", Body: "1. Read it",
	})
	if *stopped {
		t.Fatal("the agent was recycled mid-turn, which would drop the work in flight")
	}

	a.recycleIfStale()
	if !*stopped {
		t.Fatal("the turn ended and the agent was not recycled, so the skill stays invisible")
	}
	// And the freshly composed prompt is the one that carries it.
	skills, _ := LoadSkills("", a.dir)
	prompt := ComposeSystemPrompt(testProfile(), Record{ID: "boss"}, roots{own: a.dir}, "", skills)
	if !strings.Contains(prompt, "expense-filing") {
		t.Error("a rebuilt prompt does not list the new skill")
	}
	// Once taken, the flag is clear: a second turn must not recycle again.
	second, secondStopped := liveWithoutGoroutine(t, sup, "second")
	second.recycleIfStale()
	if *secondStopped {
		t.Error("an agent that wrote no skill recycled anyway")
	}
}

// A recycle refused because the person got a word in first must not throw the
// request away. The queued turn runs on the old prompt either way, but a
// consumed flag would leave the skill the agent just wrote invisible until some
// unrelated eviction -- so the request is kept and tried again at the next
// boundary.
func TestARefusedRecycleKeepsTheReloadRequest(t *testing.T) {
	sup := supervisorWith(t, 8)
	a, stopped := liveWithoutGoroutine(t, sup, "helper")
	a.reload.set()
	a.inbox <- inbound{text: "one more thing"}

	a.recycleIfStale()
	if *stopped {
		t.Fatal("an agent with a queued message was recycled; that message would never be answered")
	}
	// The next boundary, with the inbox drained, picks it up.
	<-a.inbox
	a.recycleIfStale()
	if !*stopped {
		t.Error("the reload request was lost, so the new skill stays out of the prompt")
	}
}

// A description the LOADER will discard must be refused up front. scalarValue
// reads "5" or "->" as a YAML block indicator, so accepting one meant telling
// the model "Saved", recycling the agent for nothing, and coming back with the
// skill invisible.
func TestCreateSkillRefusesADescriptionTheLoaderWouldDrop(t *testing.T) {
	builtin := t.TempDir()
	agentDir := t.TempDir()
	tools, reload := skillTools(t, builtin, agentDir)

	for _, bad := range []string{"5", "2024", "->", "-1"} {
		got := call(t, tools, "create_skill", createSkillInput{
			Name: "thing", Description: bad, Body: "steps",
		})
		if !strings.Contains(got, "description") {
			t.Errorf("description %q was accepted: %s", bad, got)
		}
	}
	if reload.take() {
		t.Error("a refused skill asked for a reload")
	}
	if skills, _ := LoadSkills(builtin, agentDir); len(skills) != 0 {
		t.Errorf("skills = %+v, want nothing written", skills)
	}
}

// One skill must not be able to push the others out of the prompt. The index
// shares a 16 kB budget, so an unbounded description evicts the built-ins.
func TestALongDescriptionCannotCrowdOutOtherSkills(t *testing.T) {
	builtin := t.TempDir()
	writeSkill(t, builtin, "pdf", "description: Read a PDF. Use when one arrives.")
	agentDir := t.TempDir()
	tools, _ := skillTools(t, builtin, agentDir)

	call(t, tools, "create_skill", createSkillInput{
		Name: "aaa-hog", Description: "Use when " + strings.Repeat("x", 40_000), Body: "steps",
	})
	skills, _ := LoadSkills(builtin, agentDir)
	out := RenderSkillsSection(skills, roots{own: agentDir, builtin: builtin})
	if !strings.Contains(out, "pdf") {
		t.Errorf("the built-in was crowded out of the prompt:\n%s", out[:min(len(out), 400)])
	}
	if len(out) > skillsCap {
		t.Errorf("section is %d bytes, over the %d budget", len(out), skillsCap)
	}
}

// Write reaches the skills folder too -- it has to, or an agent could not
// refine a skill. Without flagging a reload it is told the write succeeded
// while the skill stays out of the index until an unrelated eviction.
func TestWritingASkillDirectlyStillAsksForAReload(t *testing.T) {
	builtin := t.TempDir()
	agentDir := t.TempDir()
	tools, reload := skillTools(t, builtin, agentDir)

	call(t, tools, "Write", writeInput{
		Path:    filepath.Join(ownSkillsDir(agentDir), "by-hand", "SKILL.md"),
		Content: "---\nname: by-hand\ndescription: Written by hand. Use when testing.\n---\n\nsteps\n",
	})
	if !reload.take() {
		t.Error("a skill written with Write did not flag a reload, so it would stay invisible")
	}
	// An ordinary workspace write must NOT trigger one.
	call(t, tools, "Write", writeInput{Path: "notes.md", Content: "hello"})
	if reload.take() {
		t.Error("an ordinary file write asked for a recycle")
	}
}
