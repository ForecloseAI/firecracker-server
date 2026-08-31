package agentd

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// readCap bounds one Read result. A file larger than this would land in the
// conversation whole and be re-sent on every later turn, so it is truncated
// with a notice rather than silently cut.
const readCap = 200 << 10

// matchCap bounds how many Glob or Grep hits come back at once.
const matchCap = 200

// readInput is the Read tool's argument. The jsonschema tags are what the
// model sees: the SDK reflects over this struct to build the tool schema.
//
// Never put a comma inside a description: comma separates tag options, so the
// description is silently truncated there and the model gets a worse schema
// with no error anywhere.
type readInput struct {
	Path string `json:"path" jsonschema:"required,description=Path to the file - relative to the workspace or absolute inside it"`
}

type writeInput struct {
	Path    string `json:"path" jsonschema:"required,description=Path to write - relative to the workspace"`
	Content string `json:"content" jsonschema:"required,description=Full contents to write"`
}

type editInput struct {
	Path       string `json:"path" jsonschema:"required,description=Path to edit - relative to the workspace"`
	OldString  string `json:"old_string" jsonschema:"required,description=Exact text to replace"`
	NewString  string `json:"new_string" jsonschema:"required,description=Text to replace it with"`
	ReplaceAll bool   `json:"replace_all" jsonschema:"description=Replace every occurrence instead of requiring exactly one"`
}

type globInput struct {
	Pattern string `json:"pattern" jsonschema:"required,description=Glob pattern such as **/*.go - matched against workspace-relative paths"`
}

type grepInput struct {
	Pattern string `json:"pattern" jsonschema:"required,description=Regular expression to search for"`
	Path    string `json:"path" jsonschema:"description=Subdirectory to search - defaults to the whole workspace"`
}

// roots is where an agent's file tools may reach: the shared workspace, its
// own state directory, and the read-only built-in skills.
//
// Its own state has to be reachable or the agent cannot maintain its memory --
// the doctrine tells it to edit memory/index.md, and a tool that refuses is
// worse than no tool. Another agent's state directory is not in this list, so
// one agent still cannot read another's memory.
//
// builtin is READ-ONLY, and that asymmetry is the point. Built-in skills ship
// in the immutable rootfs and are shared by every agent on the machine, so a
// writable one would let any agent rewrite what every other agent is told to
// do. Skills an agent authors go under own, where the blast radius is itself.
type roots struct {
	workspace string // also the base for Glob and Grep walks
	own       string
	builtin   string
}

// fileTools returns the file tools, confined to the given roots.
func fileTools(r roots, reload *reloadFlag) ([]anthropic.BetaTool, error) {
	return buildTools(
		func() (anthropic.BetaTool, error) { return readTool(r) },
		func() (anthropic.BetaTool, error) { return writeTool(r, reload) },
		func() (anthropic.BetaTool, error) { return editTool(r, reload) },
		func() (anthropic.BetaTool, error) { return globTool(r) },
		func() (anthropic.BetaTool, error) { return grepTool(r) },
	)
}

// readable is the roots a path may be read from.
func (r roots) readable() []string {
	return []string{r.workspace, r.own, r.builtin}
}

// writable is the roots a path may be written to. Deliberately not readable
// minus nothing: builtin is missing, which is what makes built-in skills
// read-only.
func (r roots) writable() []string {
	return []string{r.workspace, r.own}
}

// readTool reads a text file.
func readTool(r roots) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[readInput](
		"Read", "Read a UTF-8 text file and return its contents.",
		func(ctx context.Context, in readInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			full, err := resolve(r, in.Path)
			if err != nil {
				return toolText(err.Error()), nil
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return toolText(fmt.Sprintf("could not read %s: %v", in.Path, err)), nil
			}
			return toolText(capText(string(data))), nil
		})
}

// writeTool creates or overwrites a file, making parent directories as needed.
func writeTool(r roots, reload *reloadFlag) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[writeInput](
		"Write", "Create or overwrite a file with the given contents.",
		func(ctx context.Context, in writeInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			full, err := resolveWrite(r, in.Path)
			if err != nil {
				return toolText(err.Error()), nil
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				return toolText(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(in.Content), 0o640); err != nil {
				return toolText(err.Error()), nil
			}
			noteSkillChange(r, full, reload)
			return toolText(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)), nil
		})
}

// editTool replaces exact text in a file.
func editTool(r roots, reload *reloadFlag) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[editInput](
		"Edit", "Replace exact text in a file. Fails unless old_string appears exactly once, unless replace_all is set.",
		func(ctx context.Context, in editInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			full, err := resolveWrite(r, in.Path)
			if err != nil {
				return toolText(err.Error()), nil
			}
			out := applyEdit(full, in)
			noteSkillChange(r, full, reload)
			return toolText(out), nil
		})
}

// noteSkillChange marks the prompt stale when a write lands in the agent's own
// skills folder.
//
// create_skill is the documented way in and sets this itself, but Write and
// Edit reach the same directory -- it is a writable root, so it has to be for
// an agent to refine a skill at all. Without this an agent that writes a
// SKILL.md directly is told the write succeeded and reasonably assumes the
// skill is live, when in fact it stays out of the index until some unrelated
// eviction. A body edit needs no reload, but this cannot tell the two apart
// from a path, and a redundant recycle costs one cold start.
func noteSkillChange(r roots, full string, reload *reloadFlag) {
	if dir := ownSkillsDir(r.own); dir != "" && under(full, []string{dir}) {
		reload.set()
	}
}

// applyEdit performs the replacement, reporting what happened.
//
// Requiring a unique match by default is the safeguard: a model that edits the
// first of several identical lines usually meant a different one, and a silent
// wrong edit is far worse than a failed tool call it can correct.
func applyEdit(full string, in editInput) string {
	data, err := os.ReadFile(full)
	if err != nil {
		return fmt.Sprintf("could not read %s: %v", in.Path, err)
	}
	body := string(data)
	n := strings.Count(body, in.OldString)
	if n == 0 {
		return "old_string was not found in " + in.Path
	}
	if n > 1 && !in.ReplaceAll {
		return fmt.Sprintf("old_string appears %d times in %s; pass replace_all or include more context to make it unique", n, in.Path)
	}
	if err := os.WriteFile(full, []byte(strings.ReplaceAll(body, in.OldString, in.NewString)), 0o640); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", n, in.Path)
}

// globTool lists files matching a pattern.
func globTool(r roots) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[globInput](
		"Glob", "List workspace files whose path matches a glob pattern.",
		func(ctx context.Context, in globInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			re, err := globRegexp(in.Pattern)
			if err != nil {
				return toolText("bad pattern: " + err.Error()), nil
			}
			return toolText(joinOrNone(walkMatch(r.workspace, re), "no files matched")), nil
		})
}

// walkMatch collects workspace-relative paths matching re.
func walkMatch(root string, re *regexp.Regexp) []string {
	var hits []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(hits) >= matchCap {
			return skipHidden(d, err)
		}
		if rel, rerr := filepath.Rel(root, p); rerr == nil && re.MatchString(rel) {
			hits = append(hits, rel)
		}
		return nil
	})
	return hits
}

// skipHidden keeps walks out of dot directories, which are almost never what
// was meant and can be enormous (.git most of all).
func skipHidden(d fs.DirEntry, err error) error {
	if err != nil {
		return nil // an unreadable subtree is skipped, not fatal
	}
	if d != nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
		return fs.SkipDir
	}
	return nil
}

// globRegexp converts a glob to a regexp. ** crosses directory separators, *
// and ? do not -- the usual convention, and Go's filepath.Match has no **.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
		case pattern[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// grepTool searches file contents.
func grepTool(r roots) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[grepInput](
		"Grep", "Search workspace file contents with a regular expression.",
		func(ctx context.Context, in grepInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			re, err := regexp.Compile(in.Pattern)
			if err != nil {
				return toolText("bad pattern: " + err.Error()), nil
			}
			base, err := resolve(r, orDefault(in.Path, "."))
			if err != nil {
				return toolText(err.Error()), nil
			}
			return toolText(joinOrNone(grepTree(r.workspace, base, re), "no matches")), nil
		})
}

// grepTree collects "path:line: text" hits under base.
func grepTree(root, base string, re *regexp.Regexp) []string {
	var hits []string
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(hits) >= matchCap {
			return skipHidden(d, err)
		}
		hits = append(hits, matchLines(p, relTo(root, p), re, matchCap-len(hits))...)
		return nil
	})
	return hits
}

// relTo names a hit relative to the workspace, falling back to the absolute
// path when it is not under it.
//
// Grep can now be pointed at the built-in skills, which live outside the
// workspace entirely: a plain filepath.Rel would render those as a ladder of
// "../.." that names nothing the model can then Read.
func relTo(root, p string) string {
	rel, err := filepath.Rel(root, p)
	// The separator matters: a bare ".." prefix also matches a file honestly
	// named "..notes.txt", which would then be reported by absolute path while
	// every neighbouring hit stayed relative.
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}

// matchLines returns up to limit matching lines from one file.
func matchLines(path, rel string, re *regexp.Regexp, limit int) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > readCap {
		return nil
	}
	var out []string
	for i, line := range strings.Split(string(data), "\n") {
		if len(out) >= limit {
			break
		}
		if re.MatchString(line) {
			out = append(out, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
		}
	}
	return out
}

// joinOrNone renders results, or a plain sentence when there were none. An
// empty tool result reads to the model as a failure rather than a valid answer.
func joinOrNone(lines []string, empty string) string {
	if len(lines) == 0 {
		return empty
	}
	body := strings.Join(lines, "\n")
	if len(lines) >= matchCap {
		body += fmt.Sprintf("\n[stopped at %d results - narrow the pattern]", matchCap)
	}
	return body
}

// resolve confines a path the agent wants to READ. Everything the agent touches
// goes through here or through resolveWrite, so a traversal out of the roots
// fails in one place rather than in each tool. This is a hard block, not an
// approval prompt: there is no answer a person could give that makes reaching
// into /etc correct.
//
// A relative path is taken against the workspace, which is where work happens;
// memory and skills are reached by the absolute paths the prompt already names.
func resolve(r roots, path string) (string, error) {
	full, err := absolute(r.workspace, path)
	if err != nil {
		return "", err
	}
	if under(full, r.readable()) {
		return full, nil
	}
	return "", fmt.Errorf("%s is outside the workspace and your own files", path)
}

// resolveWrite confines a path the agent wants to CHANGE.
//
// A built-in skill gets its own refusal rather than the generic one. It IS
// readable, so the generic "outside the workspace" message would contradict
// what the model just did and read as a bug; naming the rule and the way round
// it is what makes the refusal actionable.
func resolveWrite(r roots, path string) (string, error) {
	full, err := absolute(r.workspace, path)
	if err != nil {
		return "", err
	}
	if under(full, r.writable()) {
		return full, nil
	}
	if under(full, []string{r.builtin}) {
		return "", fmt.Errorf("%s is a built-in skill and is read-only - "+
			"write your own version with create_skill instead", path)
	}
	return "", fmt.Errorf("%s is outside the workspace and your own files", path)
}

// absolute anchors a relative path to the workspace and cleans it.
func absolute(workspace, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	base, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("bad workspace root: %v", err)
	}
	return filepath.Clean(filepath.Join(base, path)), nil
}

// under reports whether an absolute path sits at or inside one of the dirs.
//
// An unset root is skipped rather than filtered out by the caller. It has to
// be: filepath.Abs("") resolves to the working directory, so an agent built
// without a state dir -- which every unit test does -- would otherwise get the
// whole cwd as a root.
func under(full string, dirs []string) bool {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		base, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if full == base || strings.HasPrefix(full, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// capText truncates to readCap, telling the model it was truncated so it can
// reach for a narrower read instead of assuming it saw the whole file.
func capText(s string) string { return capTextAt(s, readCap) }

// capTextAt truncates to an arbitrary cap.
func capTextAt(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "\n[truncated: output is larger than the cap]"
}

// toolText wraps a string as a tool result content block.
func toolText(s string) anthropic.BetaToolResultBlockParamContentUnion {
	return anthropic.BetaToolResultBlockParamContentUnion{
		OfText: &anthropic.BetaTextBlockParam{Text: s},
	}
}
