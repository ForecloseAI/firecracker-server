package agentd

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed memtree
var memoryTemplates embed.FS

// memoryFileCap is the per-file injection budget, in bytes. Each always-loaded
// file is capped independently, so one runaway file cannot crowd out the other.
const memoryFileCap = 16_000

// memoryDir is where one agent's memory tree lives.
func memoryDir(agentDir string) string { return filepath.Join(agentDir, "memory") }

// instructionsPath is the agent-editable standing instructions file.
func instructionsPath(agentDir string) string {
	return filepath.Join(agentDir, "instructions.md")
}

// EnsureMemory seeds an agent's memory tree, returning what it actually wrote.
//
// Only missing files are written, and the check and the write are one syscall
// (O_EXCL), so this is both race-safe and idempotent. That is the property that
// matters: it runs on every start, and an agent's own edits must survive it.
func EnsureMemory(agentDir string) ([]string, error) {
	var wrote []string
	err := fs.WalkDir(memoryTemplates, "memtree", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		target := targetFor(agentDir, p)
		created, cerr := copyIfMissing(p, target)
		if created {
			wrote = append(wrote, target)
		}
		return cerr
	})
	return wrote, err
}

// targetFor maps a template path to where it belongs on disk. instructions.md
// sits beside the memory tree rather than inside it: it is standing behaviour
// spliced into the prompt, not a remembered fact.
func targetFor(agentDir, templatePath string) string {
	rel := strings.TrimPrefix(templatePath, "memtree/")
	if rel == "instructions.md" {
		return instructionsPath(agentDir)
	}
	return filepath.Join(memoryDir(agentDir), rel)
}

// copyIfMissing writes a template only when the target does not exist,
// reporting whether it did.
func copyIfMissing(templatePath, target string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return false, err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if os.IsExist(err) {
		return false, nil // the agent's own version stays
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	body, err := memoryTemplates.ReadFile(templatePath)
	if err != nil {
		return false, err
	}
	_, err = f.Write(body)
	return true, err
}

// RenderMemorySection builds the block that goes into the system prompt: a
// header naming the real paths, then the two always-loaded files inlined.
//
// Returns "" when neither file can be read. A broken tree should inject nothing
// rather than a block announcing that everything is unavailable, which would
// spend tokens telling the model only that it has no memory.
func RenderMemorySection(agentDir string) string {
	dir := memoryDir(agentDir)
	index := readCapped(filepath.Join(dir, "index.md"), memoryFileCap)
	definition := readCapped(filepath.Join(dir, "system", "definition.md"), memoryFileCap)
	if index == "" && definition == "" {
		return ""
	}
	return strings.Join([]string{
		memoryHeader(dir),
		"### memory/index.md", "", orDefault(index, unavailable), "",
		"### memory/system/definition.md", "", orDefault(definition, unavailable),
	}, "\n")
}

// unavailable stands in for a file that could not be read this time.
const unavailable = "(unavailable when this was loaded)"

// memoryHeader tells the model where its memory really is, so it can read
// deeper files on demand rather than only knowing what was inlined.
func memoryHeader(dir string) string {
	return strings.Join([]string{
		"## Memory",
		"",
		"These files were loaded into your context when you started:",
		"",
		"- `" + filepath.Join(dir, "index.md") + "` - your top-level index and Core Memory",
		"- `" + filepath.Join(dir, "system", "definition.md") + "` - how this memory works",
		"",
		"The files on disk are authoritative. Edit them directly, and follow links",
		"from the index when more detail is relevant. `" + dir + "` is an Open",
		"Knowledge Format (OKF) v0.1 bundle: one Markdown concept per file, opened",
		"by a short YAML frontmatter with a `type`.",
		"",
	}, "\n")
}
