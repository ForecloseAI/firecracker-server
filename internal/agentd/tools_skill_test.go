package agentd

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// skillTools builds the tool surface for an agent rooted at agentDir.
func skillTools(t *testing.T, agentDir string) ([]anthropic.BetaTool, *reloadFlag) {
	t.Helper()
	reload := &reloadFlag{}
	r := roots{workspace: t.TempDir(), own: agentDir, builtin: BuiltinSkillsDir}
	tools, err := Tools(r, toolDeps{
		gate:   NewGate(mustLog(t), NewInteractions(), t.TempDir()),
		reload: reload,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools, reload
}

// mustText pulls the text out of one tool result.
func mustText(t *testing.T, out []anthropic.BetaToolResultBlockParamContentUnion) string {
	t.Helper()
	var b strings.Builder
	for _, c := range out {
		if c.OfText != nil {
			b.WriteString(c.OfText.Text)
		}
	}
	return b.String()
}

// THE one that matters. create_skill composes the front matter itself, so the
// property to hold is that whatever it writes, the loader reads back. A skill
// the loader rejects is invisible with no error anywhere.
func TestCreateSkillRoundTripsThroughTheLoader(t *testing.T) {
	withBuiltinSkills(t)
	agentDir := t.TempDir()
	tools, reload := skillTools(t, agentDir)

	args, _ := json.Marshal(createSkillInput{
		Name:        "Expense Filing",
		Description: "File an expense.\nUse when the person sends a receipt.",
		Body:        "1. Read the receipt\n2. File it",
	})
	got := mustText(t, runTool(t, tools, "create_skill", string(args)))
	if !strings.Contains(got, "expense-filing") {
		t.Fatalf("result = %q, want the normalised name", got)
	}

	skills, problems := LoadSkills(agentDir)
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
	builtin := withBuiltinSkills(t)
	writeSkill(t, builtin, "pdf", "description: the shipped one")
	agentDir := t.TempDir()
	tools, reload := skillTools(t, agentDir)

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
		args, _ := json.Marshal(tc.in)
		got := mustText(t, runTool(t, tools, "create_skill", string(args)))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: result = %q, want it to mention %q", name, got, tc.want)
		}
	}
	if reload.take() {
		t.Error("a refused skill still asked for a reload, which would recycle the agent for nothing")
	}
	if skills, _ := LoadSkills(agentDir); len(skills) != 1 || skills[0].Description != "the shipped one" {
		t.Errorf("skills = %+v, want the built-in untouched and nothing written", skills)
	}
}

// Every profile gets create_skill whether or not it names it. An agent that
// cannot record what it worked out has to rediscover it every time.
func TestCreateSkillIsAlwaysAllowed(t *testing.T) {
	tools, _ := skillTools(t, t.TempDir())
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
	builtin := withBuiltinSkills(t)
	writeSkill(t, builtin, "pdf", "description: the shipped one")
	path := filepath.Join(builtin, "pdf", "SKILL.md")
	tools, _ := skillTools(t, t.TempDir())

	args, _ := json.Marshal(readInput{Path: path})
	if got := mustText(t, runTool(t, tools, "Read", string(args))); !strings.Contains(got, "steps go here") {
		t.Errorf("Read = %q, want the built-in's contents", got)
	}
	writeArgs, _ := json.Marshal(writeInput{Path: path, Content: "mine now"})
	if got := mustText(t, runTool(t, tools, "Write", string(writeArgs))); !strings.Contains(got, "read-only") {
		t.Errorf("Write = %q, want a refusal naming the rule", got)
	}
	editArgs, _ := json.Marshal(editInput{Path: path, OldString: "steps", NewString: "no"})
	if got := mustText(t, runTool(t, tools, "Edit", string(editArgs))); !strings.Contains(got, "read-only") {
		t.Errorf("Edit = %q, want a refusal naming the rule", got)
	}
}

// THE end-to-end of the feature inside Go: writing a skill during a turn makes
// the agent ask to be recycled once the turn is over, which is what puts the
// new skill in its prompt without a rebuild or a restart of anything.
func TestSkillWrittenDuringATurnRecyclesTheAgent(t *testing.T) {
	withBuiltinSkills(t)
	sup := supervisorWith(t, 8)
	a, stopped := liveWithoutGoroutine(t, sup, "helper")

	args, _ := json.Marshal(createSkillInput{
		Name: "expense-filing", Description: "File an expense.", Body: "1. Read it",
	})
	runTool(t, a.tools, "create_skill", string(args))
	if *stopped {
		t.Fatal("the agent was recycled mid-turn, which would drop the work in flight")
	}

	a.recycleIfStale()
	if !*stopped {
		t.Fatal("the turn ended and the agent was not recycled, so the skill stays invisible")
	}
	// And the freshly composed prompt is the one that carries it.
	if !strings.Contains(ComposeSystemPrompt(testProfile(), a.dir, ""), "expense-filing") {
		t.Error("a rebuilt prompt does not list the new skill")
	}
	// Once taken, the flag is clear: a second turn must not recycle again.
	second, secondStopped := liveWithoutGoroutine(t, sup, "second")
	second.recycleIfStale()
	if *secondStopped {
		t.Error("an agent that wrote no skill recycled anyway")
	}
}
