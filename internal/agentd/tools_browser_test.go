package agentd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"cracked/internal/agentd/cdp"
)

// fakePage stands in for Chrome. The WebSocket fake lives in package cdp, where
// the unexported message type it needs is visible, so this one works at the
// level the tools actually use: a page that navigates and can be read.
type fakePage struct {
	landed  string
	navErr  error
	takeErr error
	snap    *cdp.Snapshot
	visited []string
}

func (f *fakePage) Navigate(ctx context.Context, url string) (string, error) {
	f.visited = append(f.visited, url)
	if f.navErr != nil {
		return "", f.navErr
	}
	if f.landed != "" {
		return f.landed, nil
	}
	return url, nil
}

func (f *fakePage) Take(ctx context.Context) (*cdp.Snapshot, error) {
	if f.takeErr != nil {
		return nil, f.takeErr
	}
	return f.snap, nil
}

// browserToolsFor builds a surface around a fake page.
func browserToolsFor(t *testing.T, page pageDriver, allow []string) []anthropic.BetaTool {
	t.Helper()
	tools, err := Tools(roots{workspace: t.TempDir()},
		toolDeps{gate: NewGate(mustLog(t)), browser: page != nil, chrome: page}, allow)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// names lists what the model would be sent.
func names(tools []anthropic.BetaTool) map[string]bool {
	out := map[string]bool{}
	for _, tool := range tools {
		out[tool.Name()] = true
	}
	return out
}

// Two independent gates that can disagree is the trap. keepAllowed drops an
// unknown tool name silently rather than erroring, and all the shipped profiles
// carry explicit tools: lines -- so a browser tool name typoed by one character
// would produce an agent with browser: true, no browser tools, and no complaint
// from anywhere. The flag is the one gate; tools: governs the rest.
func TestBrowserToolsFollowTheFlagNotTheToolList(t *testing.T) {
	page := &fakePage{}
	got := names(browserToolsFor(t, page, []string{"Read"}))
	if !got["take_snapshot"] || !got["navigate_page"] {
		t.Errorf("browser tools missing though the flag is set: %v", got)
	}
	if !got["Read"] || got["Write"] {
		t.Errorf("the tools: list stopped governing the base surface: %v", got)
	}
}

// A profile without the flag must not get the browser even if it names the
// tools, or the list becomes a second way to grant something the flag is
// supposed to control.
func TestNamingBrowserToolsWithoutTheFlagGrantsNothing(t *testing.T) {
	tools, err := Tools(roots{workspace: t.TempDir()},
		toolDeps{gate: NewGate(mustLog(t)), browser: false},
		[]string{"Read", "take_snapshot", "navigate_page"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(tools); got["take_snapshot"] || got["navigate_page"] {
		t.Errorf("browser tools appeared without the flag: %v", got)
	}
}

// Most agents never open a page, so building their tools must not depend on a
// browser at all -- an accountant's first turn cannot wait on a service it has
// no use for, and must still work on a machine where Chrome never came up.
func TestNonBrowserAgentsNeedNoChrome(t *testing.T) {
	tools := browserToolsFor(t, nil, nil)
	if got := names(tools); got["take_snapshot"] {
		t.Errorf("a browserless agent got browser tools: %v", got)
	}
}

// A redirect is a finding, not a detail. A consent wall or a login gate moves
// the page, and an agent that assumes it landed where it aimed reads the wrong
// thing and reports it confidently.
func TestNavigateReportsARedirect(t *testing.T) {
	page := &fakePage{landed: "https://example.com/consent"}
	tools := browserToolsFor(t, page, nil)
	got := call(t, tools, "navigate_page", navigateInput{URL: "https://example.com"})
	if !strings.Contains(got, "redirected to https://example.com/consent") {
		t.Errorf("navigate result = %q, want the redirect called out", got)
	}
}

// Chrome's own wording is what tells the model what to do next. A failure has
// to come back as tool text, not as a Go error: an error becomes an is_error
// result the model reads as a broken tool rather than as something it can fix.
func TestBrowserFailuresComeBackAsTextNotErrors(t *testing.T) {
	page := &fakePage{takeErr: errors.New("Target closed")}
	tools := browserToolsFor(t, page, nil)
	for _, tool := range tools {
		if tool.Name() != "take_snapshot" {
			continue
		}
		blocks, err := tool.Execute(context.Background(), []byte(`{}`))
		if err != nil {
			t.Fatalf("a page failure became a Go error: %v", err)
		}
		if !strings.Contains(blocks[0].OfText.Text, "Target closed") {
			t.Errorf("result = %q, want Chrome's own wording", blocks[0].OfText.Text)
		}
	}
}

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
