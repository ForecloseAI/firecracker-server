package agentd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// reloadFlag records that an agent's prompt is now out of date.
//
// A prompt is composed once when an agent starts and then frozen, which is what
// keeps the cached prefix stable. A skill written mid-conversation therefore
// reaches the index only on the next start, so the agent asks to be recycled at
// the end of the turn. A pointer because toolDeps is copied by value into every
// tool closure, so a plain bool on the struct would be set on a dead copy --
// the same reason gate.onWait is wired onto the gate rather than into deps.
type reloadFlag struct {
	mu   sync.Mutex
	want bool
}

// set marks the prompt stale.
func (f *reloadFlag) set() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.want = true
}

// take reports whether a reload is wanted, clearing the flag.
func (f *reloadFlag) take() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := f.want
	f.want = false
	return want
}

// createSkillInput is the create_skill tool's argument.
//
// Never put a comma in a description: comma separates jsonschema tag options,
// so the text is silently truncated there and the model gets a worse schema
// with no error anywhere.
type createSkillInput struct {
	Name        string `json:"name" jsonschema:"required,description=Short lowercase hyphenated name such as expense-filing"`
	Description string `json:"description" jsonschema:"required,description=What it does AND when to reach for it - this is the only part always in your context so it decides whether you ever find it again"`
	Body        string `json:"body" jsonschema:"required,description=The procedure itself in markdown - the steps to follow when it applies"`
}

// createSkillTool saves a procedure the agent worked out, so it can follow the
// same one next time instead of rediscovering it.
func createSkillTool(r roots, d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[createSkillInput](
		"create_skill",
		"Save a procedure you worked out so you can follow it again later. Use it when you have just "+
			"done something fiddly that will come up again: the order of steps that worked, the flags "+
			"that mattered, the mistake to avoid. Not for facts about the person - those go to memory.",
		func(ctx context.Context, in createSkillInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return toolText(saveSkill(r, d, in)), nil
		})
}

// saveSkill validates and writes one skill, returning what to tell the model.
func saveSkill(r roots, d toolDeps, in createSkillInput) string {
	name := slug(in.Name)
	if refusal := checkSkill(r, name, in); refusal != "" {
		return refusal
	}
	path := filepath.Join(ownSkillsDir(r.own), name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "could not make room for that skill: " + err.Error()
	}
	if err := writeAtomic(path, []byte(composeSkill(name, in))); err != nil {
		return "could not save that skill: " + err.Error()
	}
	d.reload.set()
	logEvent(d, Event{Type: "skill", Message: "learned " + name + ": " + oneLine(in.Description)})
	return fmt.Sprintf("Saved as %q at %s. You can read it back now. It joins the skill list in "+
		"your prompt from your next message, so it is there next time this comes up.", name, path)
}

// checkSkill returns the refusal to send back, or "" when the skill is fine.
//
// A refused name is answered with the reason and the rule, never a bare no: the
// model's next move is to try again, and it can only fix what it is told.
func checkSkill(r roots, name string, in createSkillInput) string {
	switch {
	case name == "":
		return fmt.Sprintf("%q will not work as a skill name. Use lowercase letters numbers and "+
			"hyphens - something like expense-filing.", in.Name)
	case strings.TrimSpace(in.Description) == "":
		return "a skill needs a description saying what it does and when to use it. Without one it " +
			"sits on disk and never triggers, because the description is all you see until you read it."
	case strings.TrimSpace(in.Body) == "":
		return "a skill needs a body: the steps to follow when it applies."
	case ownSkillsDir(r.own) == "":
		return "there is nowhere to save a skill on this machine."
	case isBuiltinSkill(name):
		return fmt.Sprintf("%q is already a built-in skill on this machine. Read it first - if it is "+
			"genuinely missing something choose a different name for yours.", name)
	}
	return ""
}

// isBuiltinSkill reports whether a name is taken by one shipped in the image.
//
// Shadowing is possible -- an agent's own skills override built-ins by design,
// so the person can fix one that is wrong -- but it should be deliberate. A
// half-written note that silently replaced the pdf skill would be diagnosed as
// the pdf skill having broken.
func isBuiltinSkill(name string) bool {
	_, err := os.Stat(filepath.Join(BuiltinSkillsDir, name, "SKILL.md"))
	return err == nil
}

// composeSkill renders the file, so the front matter is ours rather than the
// model's. The loader is strict about it, and a skill it cannot parse is
// invisible -- letting the model hand-write the header would put that failure
// one typo away.
func composeSkill(name string, in createSkillInput) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n",
		name, oneLine(in.Description), strings.TrimSpace(in.Body))
}

// oneLine flattens a description onto a single line. The front matter parser
// reads one line per key, so a newline in here would truncate the description
// at exactly the point it stopped being visible.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
