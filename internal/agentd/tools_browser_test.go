package agentd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"cracked/internal/agentd/cdp"
)

// fakePage stands in for Chrome. The WebSocket fake lives in package cdp, where
// the unexported message type it needs is visible, so this one works at the
// level the tools actually use: a page that can be driven and read.
type fakePage struct {
	landed    string
	navErr    error
	takeErr   error
	snap      *cdp.Snapshot
	visited   []string
	gen       int
	navigates bool // the action retires the snapshot, as a real navigation does
	blocked   string
	clickErr  error
	clicked   []string
	fillErr   error
	fillGot   string
	filled    []string
	keys      []string
	waitGot   string
	waitErr   error
	dialogs   []bool
}

// advance retires the snapshot, which is what watchNavigation does for real:
// it nils it, so SnapshotGen drops to zero.
func (f *fakePage) advance() {
	if f.navigates {
		f.gen = 0
	}
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

func (f *fakePage) Click(ctx context.Context, uid string) (cdp.Target, error) {
	f.clicked = append(f.clicked, uid)
	f.advance()
	if f.clickErr != nil {
		return cdp.Target{}, f.clickErr
	}
	return cdp.Target{NodeID: 1, Role: "link", Name: "Sign in"}, nil
}

func (f *fakePage) Fill(ctx context.Context, uid, value string) (cdp.Target, string, error) {
	f.filled = append(f.filled, uid+"="+value)
	f.advance()
	if f.fillErr != nil {
		return cdp.Target{}, "", f.fillErr
	}
	got := value
	if f.fillGot != "" {
		got = f.fillGot
	}
	return cdp.Target{NodeID: 2, Role: "textbox", Name: "Email"}, got, nil
}

func (f *fakePage) PressKey(ctx context.Context, combo string) error {
	f.keys = append(f.keys, combo)
	f.advance()
	return nil
}

func (f *fakePage) WaitFor(ctx context.Context, needles []string, d time.Duration) (string, error) {
	return f.waitGot, f.waitErr
}

func (f *fakePage) HandleDialog(ctx context.Context, accept bool, text string) error {
	f.dialogs = append(f.dialogs, accept)
	f.blocked = ""
	return nil
}

func (f *fakePage) Blocked() string  { return f.blocked }
func (f *fakePage) SnapshotGen() int { return f.gen }

// browserToolsFor builds a surface around a fake page.
func browserToolsFor(t *testing.T, page *fakePage, allow []string) []anthropic.BetaTool {
	t.Helper()
	d := toolDeps{gate: NewGate(mustLog(t)), browser: page != nil}
	if page != nil {
		d.chrome = func(ctx context.Context) (browser, error) { return page, nil }
	}
	tools, err := Tools(roots{workspace: t.TempDir()}, d, allow)
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
// from anywhere. Every name in browserToolNames is asserted, not just the first
// two, because a tool built and left off that list reaches nobody.
func TestBrowserToolsFollowTheFlagNotTheToolList(t *testing.T) {
	got := names(browserToolsFor(t, &fakePage{}, []string{"Read"}))
	for _, want := range browserToolNames {
		if !got[want] {
			t.Errorf("%s is missing though the flag is set", want)
		}
	}
	if len(browserToolNames) != 7 {
		t.Errorf("browserToolNames has %d entries; update this test deliberately", len(browserToolNames))
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
		[]string{"Read", "take_snapshot", "click", "fill"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(tools)
	for _, never := range browserToolNames {
		if got[never] {
			t.Errorf("%s appeared without the flag", never)
		}
	}
}

// Most agents never open a page, so building their tools must not depend on a
// browser at all -- an accountant's first turn cannot wait on a service it has
// no use for, and must still work on a machine where Chrome never came up.
func TestNonBrowserAgentsNeedNoChrome(t *testing.T) {
	tools := browserToolsFor(t, nil, nil)
	if got := names(tools); got["take_snapshot"] || got["click"] {
		t.Errorf("a browserless agent got browser tools: %v", got)
	}
}

// The live bug this replaced: attachBrowser used to connect at agent
// construction and browserTools returned nothing when that failed. agentd is
// deliberately not ordered after chrome.service, so an agent starting during
// boot got NO browser tools for its whole life, with one line in its log. The
// tools must exist even when Chrome is currently unreachable, and the failure
// must be text the model can retry.
func TestBrowserToolsExistEvenWhenChromeIsDown(t *testing.T) {
	d := toolDeps{gate: NewGate(mustLog(t)), browser: true,
		chrome: func(ctx context.Context) (browser, error) {
			return nil, errors.New("connection refused")
		}}
	tools, err := Tools(roots{workspace: t.TempDir()}, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !names(tools)["click"] {
		t.Fatal("a browser profile lost its tools because Chrome was down at startup")
	}
	if got := call(t, tools, "click", clickInput{UID: "1_1"}); !strings.Contains(got, "connection refused") {
		t.Errorf("result = %q, want Chrome's own failure so the model can retry", got)
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
	page := &fakePage{takeErr: errors.New("Target closed"), clickErr: errors.New("No node with given id found")}
	tools := browserToolsFor(t, page, nil)
	if got := call(t, tools, "take_snapshot", snapshotInput{}); !strings.Contains(got, "Target closed") {
		t.Errorf("snapshot result = %q, want Chrome's own wording", got)
	}
	if got := call(t, tools, "click", clickInput{UID: "1_1"}); !strings.Contains(got, "No node with given id") {
		t.Errorf("click result = %q, want Chrome's own wording", got)
	}
}

// The wedge. A JavaScript dialog blocks the renderer, so getFullAXTree, every
// DOM command and every input event stop completing until it is answered.
// Sending the command anyway parks it until the deadline and burns the turn, so
// the refusal has to happen locally and no CDP may be attempted.
func TestEverythingRefusesWhileADialogIsOpen(t *testing.T) {
	page := &fakePage{blocked: `a dialog is open (confirm: "Are you sure?")`}
	tools := browserToolsFor(t, page, nil)
	for _, name := range []string{"click", "take_snapshot", "navigate_page", "press_key", "wait_for"} {
		got := callByName(t, tools, name)
		if !strings.Contains(got, "dialog is open") {
			t.Errorf("%s did not refuse: %q", name, got)
		}
	}
	if len(page.clicked)+len(page.visited)+len(page.keys) != 0 {
		t.Error("a tool reached the page while a dialog was blocking it")
	}
}

// handle_dialog is the one tool that must work while a dialog is open, because
// it is the only thing that can clear one. Routing it through the same guard as
// the rest would make the browser permanently unusable after any alert().
func TestHandleDialogWorksWhileEverythingElseIsBlocked(t *testing.T) {
	page := &fakePage{blocked: `a dialog is open (alert: "hi")`}
	tools := browserToolsFor(t, page, nil)
	got := call(t, tools, "handle_dialog", dialogInput{Action: "accept"})
	if !strings.Contains(got, "accepted the dialog") {
		t.Fatalf("handle_dialog result = %q", got)
	}
	if len(page.dialogs) != 1 || !page.dialogs[0] {
		t.Errorf("dialog handling = %v, want one accept", page.dialogs)
	}
}

// Acting tools deliberately do not re-snapshot, so the result text is the only
// thing that tells the model the page moved. Two variants, not one: a generic
// "snapshot if anything changed" on every result is noise it learns to skip.
func TestClickReportsANavigationRatherThanAGenericWarning(t *testing.T) {
	moved := browserToolsFor(t, &fakePage{gen: 3, navigates: true}, nil)
	if got := call(t, moved, "click", clickInput{UID: "3_1"}); !strings.Contains(got, "page navigated") {
		t.Errorf("result = %q, want the navigation called out", got)
	}
	still := browserToolsFor(t, &fakePage{gen: 3}, nil)
	got := call(t, still, "click", clickInput{UID: "3_1"})
	if strings.Contains(got, "page navigated") || !strings.Contains(got, "clicked link \"Sign in\"") {
		t.Errorf("result = %q, want the element named and no false navigation", got)
	}
}

// The readback is why fill costs an extra round trip. A readonly, disabled or
// maxlength-clamped field accepts every command in the sequence and keeps its
// old value, so without this the tool reports a fill that never happened.
func TestFillReportsWhenTheValueDidNotStick(t *testing.T) {
	page := &fakePage{gen: 1, fillGot: "chrome devto"}
	tools := browserToolsFor(t, page, nil)
	got := call(t, tools, "fill", fillInput{UID: "1_5", Value: "chrome devtools"})
	if !strings.Contains(got, "chrome devto") || !strings.Contains(got, "length limit") {
		t.Errorf("result = %q, want the mismatch reported", got)
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

// callByName drives a tool with a minimal valid input for its schema.
func callByName(t *testing.T, tools []anthropic.BetaTool, name string) string {
	t.Helper()
	switch name {
	case "click":
		return call(t, tools, name, clickInput{UID: "1_1"})
	case "take_snapshot":
		return call(t, tools, name, snapshotInput{})
	case "navigate_page":
		return call(t, tools, name, navigateInput{URL: "https://example.com"})
	case "press_key":
		return call(t, tools, name, pressKeyInput{Key: "Enter"})
	case "wait_for":
		return call(t, tools, name, waitForInput{Text: []string{"hi"}})
	}
	t.Fatalf("no canned input for %s", name)
	return ""
}

// The gate is decorative if there are two doors to Chrome and only one is
// watched. Chrome runs in every VM on 127.0.0.1:9222 as this same user, Node
// has a global WebSocket and puppeteer is bundled in the image, so this was
// reachable from Bash with no approval and no trace.
//
// The negative cases are half the test. The destructive list already has one
// scar from a pattern that was too broad -- `find . 2>/dev/null` stopped a live
// run dead waiting for a human -- and a bare chrome word match would fire on
// ordinary work for anyone writing about browsers.
func TestBashRedirectsAttemptsToDriveChromeDirectly(t *testing.T) {
	reach := []string{
		"curl -s http://127.0.0.1:9222/json/list",
		"curl :9222/json/version",
		`node -e 'new WebSocket("ws://127.0.0.1:9222/devtools/page/AB")'`,
		"node /opt/agent/node_modules/chrome-devtools-mcp/build/src/x.js",
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
// a right way to do this, and the model needs to be told what it is. The second
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
