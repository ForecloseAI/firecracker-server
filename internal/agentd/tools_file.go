package agentd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// readCap bounds one Read result. A file larger than this would land in the
// conversation whole and be re-sent on every later turn, so it is truncated
// with a notice rather than silently cut.
const readCap = 200 << 10

// readInput is the Read tool's argument. The jsonschema tags are what the
// model sees: the SDK reflects over this struct to build the tool schema.
type readInput struct {
	Path string `json:"path" jsonschema:"required,description=Path to the file - relative to the workspace or absolute inside it"`
}

// FileTools returns the file-reading tools, confined to root.
func FileTools(root string) ([]anthropic.BetaTool, error) {
	read, err := toolrunner.NewBetaToolFromJSONSchema[readInput](
		"Read", "Read a UTF-8 text file and return its contents.",
		func(ctx context.Context, in readInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return readFile(root, in.Path)
		})
	if err != nil {
		return nil, err
	}
	return []anthropic.BetaTool{read}, nil
}

// readFile resolves a path inside root and returns its contents as a tool
// result. A tool error is returned to the model as text, not raised to the
// caller: the model can recover from "no such file", the process cannot.
func readFile(root, path string) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	full, err := resolve(root, path)
	if err != nil {
		return toolText(err.Error()), nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return toolText(fmt.Sprintf("could not read %s: %v", path, err)), nil
	}
	return toolText(capText(string(data))), nil
}

// resolve confines a path to root. Everything the agent touches goes through
// here, so a traversal out of the workspace fails in one place rather than in
// each tool.
func resolve(root, path string) (string, error) {
	base, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("bad workspace root: %v", err)
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(base, full)
	}
	full = filepath.Clean(full)
	if full != base && !strings.HasPrefix(full, base+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the workspace", path)
	}
	return full, nil
}

// capText truncates to readCap, telling the model it was truncated so it can
// reach for a narrower read instead of assuming it saw the whole file.
func capText(s string) string {
	if len(s) <= readCap {
		return s
	}
	return s[:readCap] + "\n[truncated: file is larger than the read cap]"
}

// toolText wraps a string as a tool result content block.
func toolText(s string) anthropic.BetaToolResultBlockParamContentUnion {
	return anthropic.BetaToolResultBlockParamContentUnion{
		OfText: &anthropic.BetaTextBlockParam{Text: s},
	}
}
