package agentd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// call invokes a tool by name and returns its result text.
func call(t *testing.T, tools []anthropic.BetaTool, name string, input any) string {
	t.Helper()
	buf, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name() != name {
			continue
		}
		blocks, err := tool.Execute(context.Background(), buf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(blocks) == 0 || blocks[0].OfText == nil {
			t.Fatalf("%s returned no text block", name)
		}
		return blocks[0].OfText.Text
	}
	t.Fatalf("no tool named %q", name)
	return ""
}

// workspace builds a tool set over a populated temp workspace.
func workspace(t *testing.T) (string, []anthropic.BetaTool) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(full), 0o750)
		if err := os.WriteFile(full, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write("notes.md", "alpha\nbeta\n")
	write("src/main.go", "package main\n\nfunc main() {}\n")
	write("src/util.go", "package main\n\n// beta helper\n")
	write(".hidden/secret.go", "package hidden\n")
	tools, err := Tools(roots{workspace: root}, toolDeps{gate: NewGate(mustLog(t), NewInteractions())}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return root, tools
}

// Write then Read is the round trip everything else rests on.
func TestWriteThenRead(t *testing.T) {
	root, tools := workspace(t)
	call(t, tools, "Write", writeInput{Path: "out/hello.txt", Content: "hi there"})
	if got := call(t, tools, "Read", readInput{Path: "out/hello.txt"}); got != "hi there" {
		t.Errorf("Read = %q, want %q", got, "hi there")
	}
	if _, err := os.Stat(filepath.Join(root, "out", "hello.txt")); err != nil {
		t.Errorf("Write did not create the file: %v", err)
	}
}

// Confinement is a hard block, not a prompt, and it must hold for the writing
// tools too -- gating them would still let a wrong answer escape the workspace.
func TestWriteAndEditCannotEscapeTheWorkspace(t *testing.T) {
	_, tools := workspace(t)
	for _, path := range []string{"../escaped.txt", "/tmp/escaped.txt"} {
		got := call(t, tools, "Write", writeInput{Path: path, Content: "x"})
		if !strings.Contains(got, "outside the workspace") {
			t.Errorf("Write(%q) = %q, want an out-of-workspace refusal", path, got)
		}
	}
	got := call(t, tools, "Edit", editInput{Path: "../notes.md", OldString: "a", NewString: "b"})
	if !strings.Contains(got, "outside the workspace") {
		t.Errorf("Edit outside = %q, want a refusal", got)
	}
}

// An ambiguous edit must fail rather than guess. A model that edits the first
// of several identical lines usually meant a different one, and a silent wrong
// edit is much worse than a failed call it can correct.
func TestEditRequiresAUniqueMatch(t *testing.T) {
	_, tools := workspace(t)
	call(t, tools, "Write", writeInput{Path: "dup.txt", Content: "x\nx\nx\n"})

	got := call(t, tools, "Edit", editInput{Path: "dup.txt", OldString: "x", NewString: "y"})
	if !strings.Contains(got, "appears 3 times") {
		t.Errorf("ambiguous edit = %q, want a count and a refusal", got)
	}
	if body := call(t, tools, "Read", readInput{Path: "dup.txt"}); body != "x\nx\nx\n" {
		t.Errorf("a refused edit still changed the file: %q", body)
	}
	got = call(t, tools, "Edit", editInput{Path: "dup.txt", OldString: "x", NewString: "y", ReplaceAll: true})
	if !strings.Contains(got, "replaced 3") {
		t.Errorf("replace_all = %q, want 3 replacements", got)
	}
}

// A missing target is reported, not silently treated as a no-op.
func TestEditReportsAMissingTarget(t *testing.T) {
	_, tools := workspace(t)
	got := call(t, tools, "Edit", editInput{Path: "notes.md", OldString: "nowhere", NewString: "x"})
	if !strings.Contains(got, "not found") {
		t.Errorf("edit with no match = %q, want a not-found message", got)
	}
}

// ** has to cross directories and * has to stop at one, which is the usual
// convention and the reason filepath.Match alone is not enough.
func TestGlobDoubleStarCrossesDirectories(t *testing.T) {
	_, tools := workspace(t)
	deep := call(t, tools, "Glob", globInput{Pattern: "**/*.go"})
	if !strings.Contains(deep, "src/main.go") || !strings.Contains(deep, "src/util.go") {
		t.Errorf("**/*.go = %q, want both src files", deep)
	}
	shallow := call(t, tools, "Glob", globInput{Pattern: "*.go"})
	if strings.Contains(shallow, "src/") {
		t.Errorf("*.go = %q, want it not to cross a directory", shallow)
	}
}

// Dot directories are almost never what was meant and .git can be enormous.
func TestWalksSkipHiddenDirectories(t *testing.T) {
	_, tools := workspace(t)
	if got := call(t, tools, "Glob", globInput{Pattern: "**/*.go"}); strings.Contains(got, ".hidden") {
		t.Errorf("Glob descended into a dot directory: %q", got)
	}
	if got := call(t, tools, "Grep", grepInput{Pattern: "package"}); strings.Contains(got, ".hidden") {
		t.Errorf("Grep descended into a dot directory: %q", got)
	}
}

// Grep reports where a match is, so the model can go straight to it.
func TestGrepReportsPathAndLine(t *testing.T) {
	_, tools := workspace(t)
	got := call(t, tools, "Grep", grepInput{Pattern: "beta"})
	if !strings.Contains(got, "notes.md:2") || !strings.Contains(got, "src/util.go:3") {
		t.Errorf("Grep = %q, want path:line for both hits", got)
	}
}

// An empty result must read as a valid answer, not as a broken tool: a bare
// empty string tends to be treated by the model as a failure worth retrying.
func TestEmptyResultsSaySoInWords(t *testing.T) {
	_, tools := workspace(t)
	if got := call(t, tools, "Grep", grepInput{Pattern: "zzzz"}); got != "no matches" {
		t.Errorf("Grep with no hits = %q, want a plain sentence", got)
	}
	if got := call(t, tools, "Glob", globInput{Pattern: "*.rs"}); got != "no files matched" {
		t.Errorf("Glob with no hits = %q, want a plain sentence", got)
	}
}

// A safe command runs without troubling anyone; the gate is only for the
// destructive list.
func TestBashRunsSafeCommandsUngated(t *testing.T) {
	_, tools := workspace(t)
	if got := call(t, tools, "Bash", bashInput{Command: "echo hello"}); !strings.Contains(got, "hello") {
		t.Errorf("Bash echo = %q", got)
	}
}

// A non-zero exit is information for the model, not a failure of the turn: it
// needs the code and stderr to fix its own command.
func TestBashReportsExitStatusRatherThanFailing(t *testing.T) {
	_, tools := workspace(t)
	got := call(t, tools, "Bash", bashInput{Command: "echo oops >&2; exit 3"})
	if !strings.Contains(got, "oops") || !strings.Contains(got, "exit") {
		t.Errorf("failing command = %q, want stderr and an exit note", got)
	}
}

// Bash runs in the workspace, so relative paths mean what the model expects.
func TestBashRunsInTheWorkspace(t *testing.T) {
	root, tools := workspace(t)
	got := call(t, tools, "Bash", bashInput{Command: "pwd"})
	resolved, _ := filepath.EvalSymlinks(root)
	if !strings.Contains(got, filepath.Base(resolved)) {
		t.Errorf("pwd = %q, want it inside %q", got, root)
	}
}
