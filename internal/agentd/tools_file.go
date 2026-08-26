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

// roots is where an agent's file tools may reach: the shared workspace, and
// its own state directory.
//
// Its own state has to be reachable or the agent cannot maintain its memory --
// the doctrine tells it to edit memory/index.md, and a tool that refuses is
// worse than no tool. Another agent's state directory is not in this list, so
// one agent still cannot read another's memory.
type roots struct {
	workspace string // also the base for Glob and Grep walks
	own       string
}

// fileTools returns the file tools, confined to the given roots.
func fileTools(r roots) ([]anthropic.BetaTool, error) {
	return buildTools(
		func() (anthropic.BetaTool, error) { return readTool(r) },
		func() (anthropic.BetaTool, error) { return writeTool(r) },
		func() (anthropic.BetaTool, error) { return editTool(r) },
		func() (anthropic.BetaTool, error) { return globTool(r) },
		func() (anthropic.BetaTool, error) { return grepTool(r) },
	)
}

// list is the roots a path may resolve under, skipping any that is unset.
func (r roots) list() []string {
	out := []string{r.workspace}
	if r.own != "" {
		out = append(out, r.own)
	}
	return out
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
func writeTool(r roots) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[writeInput](
		"Write", "Create or overwrite a file with the given contents.",
		func(ctx context.Context, in writeInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			full, err := resolve(r, in.Path)
			if err != nil {
				return toolText(err.Error()), nil
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				return toolText(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(in.Content), 0o640); err != nil {
				return toolText(err.Error()), nil
			}
			return toolText(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)), nil
		})
}

// editTool replaces exact text in a file.
func editTool(r roots) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[editInput](
		"Edit", "Replace exact text in a file. Fails unless old_string appears exactly once, unless replace_all is set.",
		func(ctx context.Context, in editInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			full, err := resolve(r, in.Path)
			if err != nil {
				return toolText(err.Error()), nil
			}
			return toolText(applyEdit(full, in)), nil
		})
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
		rel, _ := filepath.Rel(root, p)
		hits = append(hits, matchLines(p, rel, re, matchCap-len(hits))...)
		return nil
	})
	return hits
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

// resolve confines a path to the agent's roots. Everything the agent touches
// goes through here, so a traversal out of them fails in one place rather than
// in each tool. This is a hard block, not an approval prompt: there is no
// answer a person could give that makes reaching into /etc correct.
//
// A relative path is taken against the workspace, which is where work happens;
// memory is reached by the absolute path the prompt already names.
func resolve(r roots, path string) (string, error) {
	full := path
	if !filepath.IsAbs(full) {
		base, err := filepath.Abs(r.workspace)
		if err != nil {
			return "", fmt.Errorf("bad workspace root: %v", err)
		}
		full = filepath.Join(base, full)
	}
	full = filepath.Clean(full)
	for _, root := range r.list() {
		base, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if full == base || strings.HasPrefix(full, base+string(filepath.Separator)) {
			return full, nil
		}
	}
	return "", fmt.Errorf("%s is outside the workspace and your own files", path)
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
