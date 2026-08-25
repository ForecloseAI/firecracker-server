package agentd

import (
	"strings"
	"testing"
)

// The snapshot-then-act contract is only stated in the prompt. Without it the
// model invents uids, and an invented uid is indistinguishable from a stale one
// -- both come back as the same refusal, so it cannot tell which it made.
func TestBrowserProfilesGetTheSnapshotRuleInTheirPrompt(t *testing.T) {
	dir := t.TempDir()
	with := ComposeSystemPrompt(Profile{Browser: true}, dir)
	without := ComposeSystemPrompt(Profile{Browser: false}, dir)
	if !strings.Contains(with, "Take a snapshot before you act") {
		t.Error("a browser profile was not told the snapshot rule")
	}
	if strings.Contains(without, "Take a snapshot before you act") {
		t.Error("a browserless profile is carrying browser guidance it cannot use")
	}
}

// The gate is decorative if there are two doors to Chrome and only one is
// watched. Chrome runs in every VM on 127.0.0.1:9222 as this same user, Node
// has a global WebSocket, and the MCP server's own bundle is on disk -- so this
// was reachable from Bash with no approval and no trace. It matters more now,
// not less: the shell must not reach the very server the daemon owns.
//
// The negative cases are half the test. The destructive list already has one
// scar from a pattern that was too broad -- `find . 2>/dev/null` stopped a live
// run dead waiting for a human.
func TestBashRedirectsAttemptsToDriveChromeDirectly(t *testing.T) {
	reach := []string{
		"curl -s http://127.0.0.1:9222/json/list",
		"curl :9222/json/version",
		`node -e 'new WebSocket("ws://127.0.0.1:9222/devtools/page/AB")'`,
		"node /opt/agent/node_modules/chrome-devtools-mcp/build/src/bin/chrome-devtools-mcp.js",
		"google-chrome --headless --dump-dom https://example.com",
		"pkill -f chromium",
		"xdotool key Return",
	}
	for _, cmd := range reach {
		if !reachesBrowser(cmd) {
			t.Errorf("not caught: %s", cmd)
		}
	}
	fine := []string{
		"find . -name '*.go' 2>/dev/null",
		"grep -r chrome /var/log",
		"echo 'chrome is slow' >> notes.md",
		"ls ~/.config/chromium",
		"go test ./...",
	}
	for _, cmd := range fine {
		if reachesBrowser(cmd) {
			t.Errorf("false positive on ordinary work: %s", cmd)
		}
	}
}

// A redirect, not a refusal and not an approval prompt: unlike rm -rf there is
// a right way to do this and the model needs to be told what it is. The second
// half matters too -- `puppeteer` also matches a coder installing it for the
// person's own project, and that task must not dead-end.
func TestTheBrowserRedirectNamesTheToolsAndLeavesADoorOpen(t *testing.T) {
	tools, err := Tools(roots{workspace: t.TempDir()}, toolDeps{gate: NewGate(mustLog(t))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := call(t, tools, "Bash", bashInput{Command: "curl http://127.0.0.1:9222/json/list"})
	if !strings.Contains(got, "navigate_page") || !strings.Contains(got, "take_snapshot") {
		t.Errorf("result = %q, want it to name the browser tools", got)
	}
	if !strings.Contains(got, "ask them first") {
		t.Error("a legitimate automation-coding task is left with no way forward")
	}
}

// browser: true is the single gate. A profile that names browser tools in its
// tools: list without the flag must get none of them, or the list becomes a
// second way to grant something the flag is supposed to control.
func TestNamingBrowserToolsWithoutTheFlagGrantsNothing(t *testing.T) {
	tools, err := Tools(roots{workspace: t.TempDir()},
		toolDeps{gate: NewGate(mustLog(t)), browser: false},
		[]string{"Read", "take_snapshot", "click", "fill"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if browserAllowed[tool.Name()] {
			t.Errorf("%s appeared without the flag", tool.Name())
		}
	}
}

// withBrowser and the tools/list filter must read the same list, or a name in
// one and not the other is a tool that reaches nobody with nothing erroring.
// That drift is why this is one map rather than two slices.
func TestTheAllowListIsTheOnlySourceOfBrowserToolNames(t *testing.T) {
	got := withBrowser([]string{"Read"}, true)
	for name := range browserAllowed {
		if !contains(got, name) {
			t.Errorf("%s is allowed by the filter but never reaches keepAllowed", name)
		}
	}
	if contains(withBrowser([]string{"Read"}, false), "click") {
		t.Error("a browserless profile had browser names added to its allow list")
	}
}

// contains reports whether a slice holds a string.
func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
