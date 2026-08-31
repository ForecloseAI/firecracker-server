package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withBuiltinSkills points the built-in skills directory at a temp dir for the
// duration of one test. BuiltinSkillsDir is a package var for exactly this, the
// way ChromeURL is, and restoring it matters: tests share a process.
func withBuiltinSkills(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	was := BuiltinSkillsDir
	BuiltinSkillsDir = dir
	t.Cleanup(func() { BuiltinSkillsDir = was })
	return dir
}

// writeSkill puts one SKILL.md on disk with the given front matter and body.
func writeSkill(t *testing.T, dir, name, front string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(full, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\n" + front + "\n---\n\nsteps go here\n"
	if err := os.WriteFile(filepath.Join(full, "SKILL.md"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

// The ordinary case: a well-formed skill is found, and only its description is
// carried, because the body is what stays on disk until it is needed.
func TestLoadSkillsReadsBuiltinsAndOwn(t *testing.T) {
	builtin := withBuiltinSkills(t)
	agentDir := t.TempDir()
	writeSkill(t, builtin, "pdf", "name: pdf\ndescription: Read a PDF. Use when the person sends one.")
	writeSkill(t, ownSkillsDir(agentDir), "expense-filing", "name: expense-filing\ndescription: File an expense.")

	skills, problems := LoadSkills(agentDir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(skills) != 2 || skills[0].Name != "expense-filing" || skills[1].Name != "pdf" {
		t.Fatalf("skills = %+v, want expense-filing and pdf in name order", skills)
	}
	if !strings.HasPrefix(skills[1].Description, "Read a PDF") {
		t.Errorf("description = %q, want the front matter's", skills[1].Description)
	}
}

// An agent's own skill wins over a built-in of the same name, so a person can
// fix one that is wrong on their machine without a new image.
func TestOwnSkillOverridesBuiltin(t *testing.T) {
	builtin := withBuiltinSkills(t)
	agentDir := t.TempDir()
	writeSkill(t, builtin, "pdf", "description: the shipped one")
	writeSkill(t, ownSkillsDir(agentDir), "pdf", "description: the local one")

	skills, _ := LoadSkills(agentDir)
	if len(skills) != 1 {
		t.Fatalf("skills = %+v, want one", skills)
	}
	if skills[0].Description != "the local one" {
		t.Errorf("description = %q, want the agent's own to win", skills[0].Description)
	}
	if !strings.HasPrefix(skills[0].Path, agentDir) {
		t.Errorf("path = %q, want it under the agent's own directory", skills[0].Path)
	}
}

// THE one that matters for authoring. A skill with no description is invisible
// to the model and nothing errors, so it looks exactly like a skill whose
// description never matched. Being skipped AND named is what makes it findable.
func TestSkillWithoutDescriptionIsSkippedAndReported(t *testing.T) {
	withBuiltinSkills(t)
	agentDir := t.TempDir()
	writeSkill(t, ownSkillsDir(agentDir), "silent", "name: silent")
	writeSkill(t, ownSkillsDir(agentDir), "folded", "description: >")

	skills, problems := LoadSkills(agentDir)
	if len(skills) != 0 {
		t.Fatalf("skills = %+v, want none loadable", skills)
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want one per broken skill", problems)
	}
	for _, want := range []string{"silent", "folded"} {
		if !strings.Contains(strings.Join(problems, " "), want) {
			t.Errorf("problems = %v, want %s named", problems, want)
		}
	}
}

// A directory beside a skill is not a broken skill. Skills bundle scripts/ and
// references/, and reporting those would bury the one report that matters.
func TestDirectoryWithoutSkillFileIsNotAProblem(t *testing.T) {
	builtin := withBuiltinSkills(t)
	if err := os.MkdirAll(filepath.Join(builtin, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	skills, problems := LoadSkills("")
	if len(skills) != 0 || len(problems) != 0 {
		t.Errorf("skills = %+v problems = %v, want both empty", skills, problems)
	}
}

// The prompt carries descriptions and paths, never bodies. That is the whole
// economy of the feature: many skills cost many lines, not many procedures.
func TestRenderSkillsSectionListsWithoutBodies(t *testing.T) {
	builtin := withBuiltinSkills(t)
	writeSkill(t, builtin, "pdf", "description: Read a PDF.")

	out := RenderSkillsSection(t.TempDir())
	for _, want := range []string{"pdf", "Read a PDF.", filepath.Join(builtin, "pdf", "SKILL.md")} {
		if !strings.Contains(out, want) {
			t.Errorf("section is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "steps go here") {
		t.Error("the body was inlined; only name and description belong in the prompt")
	}
}

// A machine with no skills at all injects nothing, rather than a block whose
// only content is that there is nothing. Same rule the memory section follows.
func TestRenderSkillsSectionIsEmptyWithNoSkills(t *testing.T) {
	withBuiltinSkills(t)
	if out := RenderSkillsSection(t.TempDir()); out != "" {
		t.Errorf("section = %q, want empty", out)
	}
}

// The header carries an exception to BaseLimits, which says text you read is
// information rather than instruction. It has to name the directories it
// covers, or it reads as permission to follow anything found anywhere.
func TestSkillsHeaderScopesTheInstructionException(t *testing.T) {
	builtin := withBuiltinSkills(t)
	agentDir := t.TempDir()
	writeSkill(t, builtin, "pdf", "description: Read a PDF.")

	out := RenderSkillsSection(agentDir)
	if !strings.Contains(out, builtin) || !strings.Contains(out, ownSkillsDir(agentDir)) {
		t.Errorf("header does not name both skill directories:\n%s", out)
	}
	if !strings.Contains(out, "anywhere else") {
		t.Errorf("header does not bound the exception:\n%s", out)
	}
}

// Every skill this image ships has to survive its own loader. They are the
// files that teach the format, so a typo in one teaches the wrong format -- and
// a description that will not parse means the skill silently never triggers.
func TestShippedSkillsAllParse(t *testing.T) {
	dir := filepath.Join("..", "..", "rootfs", "files", "skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no skills ship with the image")
	}
	for _, e := range entries {
		buf, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		s, err := parseSkill(string(buf), e.Name())
		if err != nil {
			t.Errorf("%s does not parse: %v", e.Name(), err)
			continue
		}
		// The description is the only part ever loaded, so it has to carry both
		// what the skill does and when to reach for it. A bare label ("Handles
		// invoices.") triggers nothing, and that failure is silent -- an unread
		// skill looks exactly like one that had no match.
		if len(s.Description) < 60 || !strings.Contains(strings.ToLower(s.Description), "when") {
			t.Errorf("%s description does not say when to reach for it: %q", e.Name(), s.Description)
		}
	}
}
