package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"cracked/internal/agentapi"
)

// personCap is the injection budget for what we know about the person, in bytes.
// The same as a memory file: this sits in every agent's prefix and is paid for on
// every turn, so it is allowed to grow but not without a limit.
const personCap = 16_000

// personPath is where the person's profile lives.
//
// One file for the whole machine, not one per agent. Everyone here works for the
// same person, and a fact learned by one is a fact all of them should have --
// keeping a copy per agent would mean the boss knowing a name the accountant does
// not.
func personPath(stateDir string) string {
	return filepath.Join(stateDir, "about-the-person.md")
}

// ReadPerson returns the profile, or "" when there is not one yet.
func ReadPerson(stateDir string) string {
	return readCapped(personPath(stateDir), personCap)
}

// WritePerson replaces the profile with what onboarding collected.
func WritePerson(stateDir string, p agentapi.Person) error {
	body := renderPerson(p)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(personPath(stateDir), []byte(body), 0o640)
}

// renderPerson lays the profile out as Markdown, because that is what goes into
// the prompt: no parsing on the way back out, and a person can read the file.
func renderPerson(p agentapi.Person) string {
	lines := []string{"# " + orDefault(p.Name, "The person"), ""}
	if p.Name != "" {
		lines = append(lines, "- They go by **"+p.Name+"**. Call them that.")
	}
	if p.Work != "" {
		lines = append(lines, "- What they do: "+p.Work)
	}
	lines = append(lines, "", "## What we have learned", "")
	if p.Notes != "" {
		lines = append(lines, p.Notes)
	}
	return strings.Join(lines, "\n") + "\n"
}

// AppendAboutPerson adds one learned fact to the profile.
//
// Append-only and dated. An agent rewriting the file wholesale would eventually
// drop something another agent put there, and the two never see each other's
// turns; adding a line is the only edit that is safe from several of them at
// once.
func AppendAboutPerson(stateDir, fact, by string, now time.Time) error {
	if strings.TrimSpace(fact) == "" {
		return nil
	}
	line := "- " + strings.TrimSpace(fact) + " _(" + by + ", " + now.Format("2006-01-02") + ")_\n"
	f, err := os.OpenFile(personPath(stateDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// RenderPersonSection is the block spliced into every agent's system prompt.
func RenderPersonSection(stateDir string) string {
	body := ReadPerson(stateDir)
	if body == "" {
		return ""
	}
	return "## About the person you work for\n\n" + body +
		"\nWhen you learn something durable about them, record it with " +
		"remember_about_person. Passing preferences for one task are not worth keeping."
}
