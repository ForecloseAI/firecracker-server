package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seed runs on every start, so it must never overwrite what the agent has
// written. This is the property the whole subsystem depends on: memory that a
// restart quietly resets is not memory.
func TestScaffoldNeverClobbersWhatTheAgentWrote(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureMemory(dir); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(memoryDir(dir), "index.md")
	os.WriteFile(index, []byte("# Mine\n\nTheir dog is called Ada.\n"), 0o640)

	wrote, err := EnsureMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 0 {
		t.Errorf("a second seed rewrote %v, want nothing", wrote)
	}
	body, _ := os.ReadFile(index)
	if !strings.Contains(string(body), "Ada") {
		t.Error("the agent's own index was overwritten by the seed")
	}
}

// A fresh agent must end up with a usable tree, and instructions.md must land
// beside it rather than inside it: one is standing behaviour, the other is
// remembered fact.
func TestScaffoldWritesTheExpectedTree(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureMemory(dir); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"instructions.md",
		"memory/index.md",
		"memory/system/index.md",
		"memory/system/definition.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}

// A restored file the agent deleted should come back, so a half-deleted tree
// heals instead of staying broken.
func TestScaffoldRestoresADeletedFile(t *testing.T) {
	dir := t.TempDir()
	EnsureMemory(dir)
	os.Remove(filepath.Join(memoryDir(dir), "system", "definition.md"))

	wrote, err := EnsureMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 || !strings.HasSuffix(wrote[0], "definition.md") {
		t.Errorf("re-seeded %v, want just the deleted definition", wrote)
	}
}

// The block must name the real paths on disk, so the agent can read deeper
// files rather than knowing only what was inlined.
func TestMemorySectionNamesRealPathsAndInlinesBothFiles(t *testing.T) {
	dir := t.TempDir()
	EnsureMemory(dir)

	got := RenderMemorySection(dir)
	if !strings.Contains(got, filepath.Join(memoryDir(dir), "index.md")) {
		t.Error("the header does not name the real index path")
	}
	if !strings.Contains(got, "### memory/index.md") ||
		!strings.Contains(got, "### memory/system/definition.md") {
		t.Error("a section heading is missing")
	}
	if !strings.Contains(got, "Open Knowledge Format") {
		t.Error("the definition was not actually inlined")
	}
}

// A broken tree must inject nothing at all. A block that only says everything
// is unavailable spends tokens telling the model it has no memory.
func TestMemorySectionIsEmptyWhenNothingIsReadable(t *testing.T) {
	if got := RenderMemorySection(t.TempDir()); got != "" {
		t.Errorf("an unseeded tree rendered %q, want nothing", got)
	}
}

// One runaway file must not crowd out the other, so each is capped on its own.
func TestMemoryFilesAreCappedIndependently(t *testing.T) {
	dir := t.TempDir()
	EnsureMemory(dir)
	huge := strings.Repeat("x", memoryFileCap*2)
	os.WriteFile(filepath.Join(memoryDir(dir), "index.md"), []byte(huge), 0o640)

	got := RenderMemorySection(dir)
	if !strings.Contains(got, "[truncated") {
		t.Error("an oversized index was inlined whole")
	}
	if !strings.Contains(got, "Open Knowledge Format") {
		t.Error("an oversized index displaced the definition, which is capped separately")
	}
}

// Memory reaches the model through the system prompt, and the limits still
// close it -- memory is data the agent controls, so it must not come last.
func TestMemoryLandsInThePromptBeforeTheLimits(t *testing.T) {
	dir := t.TempDir()
	EnsureMemory(dir)

	got := ComposeSystemPrompt(Profile{Prompt: "role"}, Record{}, roots{own: dir}, "", nil)
	mem := strings.Index(got, "## Memory")
	limits := strings.Index(got, BaseLimits)
	if mem < 0 {
		t.Fatal("the memory block never reached the prompt")
	}
	if mem > limits {
		t.Error("memory was composed after the limits")
	}
}

// Each agent's memory is its own. Two agents sharing a tree would leak one
// person's facts into another agent's context.
func TestAgentsDoNotShareAMemoryTree(t *testing.T) {
	root := t.TempDir()
	one, two := filepath.Join(root, "ada"), filepath.Join(root, "bo")
	EnsureMemory(one)
	EnsureMemory(two)
	os.WriteFile(filepath.Join(memoryDir(one), "index.md"), []byte("ada's private note"), 0o640)

	if strings.Contains(RenderMemorySection(two), "private note") {
		t.Error("one agent's memory leaked into another's")
	}
}

// An agent must be able to read and write its own memory. Its doctrine tells it
// to edit memory/index.md, so a tool that refuses to touch its own state dir
// makes the whole subsystem inert -- which is exactly what happened the first
// time this ran against a live model.
func TestAgentCanReachItsOwnMemoryButNotAnotherAgents(t *testing.T) {
	root := t.TempDir()
	mine, theirs := filepath.Join(root, "ada"), filepath.Join(root, "bo")
	EnsureMemory(mine)
	EnsureMemory(theirs)
	r := roots{workspace: t.TempDir(), own: mine}

	if _, err := resolve(r, filepath.Join(memoryDir(mine), "index.md")); err != nil {
		t.Errorf("an agent could not reach its own memory: %v", err)
	}
	if _, err := resolve(r, filepath.Join(memoryDir(theirs), "index.md")); err == nil {
		t.Error("an agent reached another agent's memory")
	}
	if _, err := resolve(r, "/etc/passwd"); err == nil {
		t.Error("confinement did not hold for an unrelated absolute path")
	}
}
