package agentd

import (
	"github.com/anthropics/anthropic-sdk-go"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped profiles are the product, so their absence or malformation
// should fail here rather than at an agent's first turn.
func TestBuiltinProfilesAllParse(t *testing.T) {
	c, err := LoadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"boss", "coder", "marketer", "analyst", "accountant", "researcher"} {
		p, ok := c.Get(key)
		if !ok {
			t.Errorf("no built-in profile %q", key)
			continue
		}
		if p.Title == "" || p.Description == "" || p.Model == "" {
			t.Errorf("%s is missing front matter: %+v", key, p)
		}
		if len(p.Tools) == 0 {
			t.Errorf("%s declares no tools", key)
		}
		if len(p.Prompt) < 200 {
			t.Errorf("%s has a suspiciously short role prompt (%d bytes)", key, len(p.Prompt))
		}
	}
}

// A profile written to disk overrides a built-in of the same key, which is how
// an agent's role can be changed without a rebuild.
func TestCustomProfileOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "coder.md"), []byte(
		"---\nkey: coder\ntitle: My Coder\nmodel: claude-haiku-4-5\ntools: Read\n---\n\nDo it my way.\n"), 0o640)

	c, err := LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Get("coder")
	if p.Title != "My Coder" || p.Model != "claude-haiku-4-5" || p.Prompt != "Do it my way." {
		t.Errorf("custom profile did not override the built-in: %+v", p)
	}
	if _, ok := c.Get("boss"); !ok {
		t.Error("a custom directory removed the other built-ins")
	}
}

// A missing custom directory is the normal case on a fresh machine.
func TestMissingCustomDirectoryKeepsBuiltins(t *testing.T) {
	c, err := LoadCatalog(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.List()) != 7 {
		t.Errorf("got %d profiles, want the 7 built-ins", len(c.List()))
	}
}

// The custom profile is the shell a person's own role goes into: every tool,
// the browser, and a role prompt that only points at what they wrote.
func TestTheCustomProfileIsAShellForThePersonsRole(t *testing.T) {
	c, _ := LoadCatalog("")
	p, ok := c.Get(CustomType)
	if !ok || !p.Browser || len(p.Tools) != 0 || p.Model == "" {
		t.Fatalf("custom profile: %+v, %v", p, ok)
	}
}

// Front matter is parsed by hand, so the field types it supports are worth
// pinning: a list, a bool, and the body surviving intact.
func TestParseProfileReadsEveryFieldType(t *testing.T) {
	p, err := parseProfile("---\nkey: k\ntitle: T\ndescription: D\nmodel: m\n" +
		"browser: true\ntools: Read, Write , Bash\n---\n\n## Role\nBody text.\n")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Browser {
		t.Error("browser: true did not parse")
	}
	if strings.Join(p.Tools, "|") != "Read|Write|Bash" {
		t.Errorf("tools = %v, want three trimmed names", p.Tools)
	}
	if p.Prompt != "## Role\nBody text." {
		t.Errorf("prompt = %q, want the body with its heading", p.Prompt)
	}
}

// A file without front matter is a mistake, not a profile with an empty key.
func TestParseProfileRejectsMissingFrontMatter(t *testing.T) {
	if _, err := parseProfile("just some markdown\n"); err == nil {
		t.Error("a file with no front matter parsed as a profile")
	}
	if _, err := parseProfile("---\nkey: k\nnever closed\n"); err == nil {
		t.Error("unterminated front matter parsed as a profile")
	}
}

// A profile's tool list is a real restriction, not advice: a tool it does not
// name is never sent to the model, so it cannot be called at all. This is what
// will make boss-only powers structural.
func TestProfileToolListNarrowsTheSurface(t *testing.T) {
	gate := NewGate(mustLog(t), NewInteractions(), t.TempDir())
	all, err := Tools(roots{workspace: t.TempDir()}, toolDeps{gate: gate}, nil)
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := Tools(roots{workspace: t.TempDir()}, toolDeps{gate: gate}, []string{"Read", "Grep"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) <= len(narrow) {
		t.Fatalf("narrowing did not reduce the surface: %d vs %d", len(all), len(narrow))
	}
	for _, tool := range narrow {
		// alwaysAllowed is the deliberate exception: those tools belong to every
		// agent whatever its profile says, so a narrowed set still carries them.
		if tool.Name() != "Read" && tool.Name() != "Grep" && !contains(alwaysAllowed, tool.Name()) {
			t.Errorf("narrowed set contains %q", tool.Name())
		}
	}
	for _, name := range alwaysAllowed {
		if !hasTool(narrow, name) {
			t.Errorf("%s is always allowed but a narrow profile did not get it", name)
		}
	}
}

// hasTool reports whether a surface offers a tool by name.
func hasTool(tools []anthropic.BetaTool, want string) bool {
	for _, t := range tools {
		if t.Name() == want {
			return true
		}
	}
	return false
}
