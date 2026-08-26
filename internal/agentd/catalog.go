package agentd

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cracked/internal/agentapi"
)

//go:embed profiles/*.md
var builtinProfiles embed.FS

// Profile is one kind of agent: its role prompt, its model, and what it may do.
// Shipped as markdown with a small front matter block, so adding a specialist
// is writing a file rather than changing code. The shape lives in agentapi
// because the host renders a roster from it.
type Profile = agentapi.Profile

// Catalog is the set of profiles an agent can be created from: the built-in
// ones compiled into the binary, plus any written to disk, which override a
// built-in of the same key.
type Catalog struct {
	byKey map[string]Profile
}

// LoadCatalog reads the built-in profiles and then any custom ones in dir.
// A missing or unreadable custom directory is the normal case, not an error.
func LoadCatalog(dir string) (*Catalog, error) {
	c := &Catalog{byKey: map[string]Profile{}}
	if err := c.absorb(builtinProfiles, "profiles"); err != nil {
		return nil, fmt.Errorf("built-in profiles: %w", err)
	}
	if dir != "" {
		c.absorb(os.DirFS(dir), ".")
	}
	return c, nil
}

// absorb parses every .md file in one directory of a filesystem.
func (c *Catalog) absorb(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		buf, err := fs.ReadFile(fsys, filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if p, err := parseProfile(string(buf)); err == nil && p.Key != "" {
			c.byKey[p.Key] = p
		}
	}
	return nil
}

// Get returns one profile by key.
func (c *Catalog) Get(key string) (Profile, bool) {
	p, ok := c.byKey[key]
	return p, ok
}

// List returns every profile, ordered by key so the API is stable.
func (c *Catalog) List() []Profile {
	out := make([]Profile, 0, len(c.byKey))
	for _, p := range c.byKey {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// parseProfile splits a profile file into front matter and role prompt.
//
// The front matter is deliberately not YAML. It is half a dozen scalar keys,
// and the only YAML parser available here arrived as a transitive dependency of
// the Anthropic SDK -- depending on that directly would tie this file to
// somebody else's dependency graph for no gain.
func parseProfile(text string) (Profile, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), "---")
	if !ok {
		return Profile{}, fmt.Errorf("missing front matter")
	}
	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Profile{}, fmt.Errorf("unterminated front matter")
	}
	p := Profile{Prompt: strings.TrimSpace(body)}
	for _, line := range strings.Split(front, "\n") {
		applyField(&p, line)
	}
	return p, nil
}

// applyField sets one "key: value" line on a profile, ignoring anything else.
func applyField(p *Profile, line string) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch key {
	case "key":
		p.Key = value
	case "title":
		p.Title = value
	case "description":
		p.Description = value
	case "model":
		p.Model = value
	case "browser":
		p.Browser = value == "true"
	case "tools":
		p.Tools = splitList(value)
	}
}

// splitList reads a comma-separated list, dropping empties.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(strings.Trim(value, "[]"), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
