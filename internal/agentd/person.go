package agentd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
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
	return "## About the person you work for\n\n" + body + whereTheyAre(stateDir) +
		"\nWhen you learn something durable about them, record it with " +
		"remember_about_person. Passing preferences for one task are not worth keeping."
}

// whereTheyAre is the fact the language rule in the base identity turns on:
// their country when the app sent one, or, for a machine onboarded before it
// did, their clock, which is the nearest thing to a region it knows.
func whereTheyAre(stateDir string) string {
	if code := readStateFile(countryPath(stateDir)); code != "" {
		return "- Country: " + code + "\n"
	}
	if tz := readStateFile(zonePath(stateDir)); tz != "" {
		return "- Their clock is " + tz + ".\n"
	}
	return ""
}

// countryPath is where the person's country is remembered: machine state
// beside the zone, for the zone's reasons.
func countryPath(stateDir string) string { return filepath.Join(stateDir, "country") }

// countryShape is what the app's country list is keyed on: two capital
// letters, an ISO 3166-1 alpha-2 code. Only the shape is checked here; the
// list is the app's, and a code it does not know costs a line the model
// cannot use, not a wrong clock.
var countryShape = regexp.MustCompile(`^[A-Z]{2}$`)

// validCountry says why a code cannot be stored, and is the one place that
// rule lives, so the handler refuses exactly what would not be kept.
func validCountry(code string) error {
	if !countryShape.MatchString(code) {
		return errors.New("country must be a two-letter ISO code")
	}
	return nil
}

// rememberCountry stores a code, reporting whether it changed.
func rememberCountry(stateDir, code string) bool {
	if validCountry(code) != nil {
		return false
	}
	return storeStateFile(countryPath(stateDir), code)
}

// readStateFile is one machine-state value -- the zone, the country -- read
// back, or "" when there is not one yet. Trimmed so no caller has to defend
// against the trailing newline a hand edit of the file would leave.
func readStateFile(path string) string {
	buf, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf))
}

// storeStateFile writes one machine-state value, reporting whether it changed:
// the same value again writes nothing, which is what tells a caller that
// nothing has to be recomposed.
func storeStateFile(path, value string) bool {
	if value == readStateFile(path) {
		return false
	}
	return os.WriteFile(path, []byte(value), 0o640) == nil
}
