package agentd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"cracked/internal/agentd/cdp"
)

// Inputs for the browser tools. Descriptions must contain no commas: comma
// separates jsonschema tag options, so one would silently truncate the text the
// model sees.
type navigateInput struct {
	URL string `json:"url" jsonschema:"required,description=Absolute URL to open in the browser"`
}

type snapshotInput struct{}

// browserToolNames is the family this profile flag switches on.
//
// It is the single registration point and the failure is silent both ways:
// withBrowser appends exactly this list to a profile's allow list and
// keepAllowed drops any name it does not recognise without complaining. A tool
// built and left out of here reaches nobody, and nothing anywhere errors.
var browserToolNames = []string{
	"navigate_page", "take_snapshot", "click", "fill", "press_key",
	"wait_for", "handle_dialog",
}

// browser is the slice of Chrome these tools use.
//
// An interface so this package's tests can stand a page up in memory: the
// WebSocket fake lives in package cdp, where the unexported message type it
// needs is visible, and standing up a socket to test a string renderer would be
// absurd. *cdp.Browser satisfies this as written.
type browser interface {
	Navigate(ctx context.Context, url string) (string, error)
	Take(ctx context.Context) (*cdp.Snapshot, error)
	Click(ctx context.Context, uid string) (cdp.Target, error)
	Fill(ctx context.Context, uid, value string) (cdp.Target, string, error)
	PressKey(ctx context.Context, combo string) error
	WaitFor(ctx context.Context, needles []string, d time.Duration) (string, error)
	HandleDialog(ctx context.Context, accept bool, promptText string) error
	Blocked() string
	SnapshotGen() int
}

// openBrowser hands back the machine's one Chrome, connecting on first use.
type openBrowser func(ctx context.Context) (browser, error)

// attachBrowser gives an agent its snapshot store and a way to reach Chrome.
//
// It does NOT connect. Resolving per call instead is the whole point: agentd
// deliberately does not order itself after chrome.service, so an agent that
// starts during boot would otherwise be built with no browser tools and keep
// none for its entire life, with a single line in its log. And the supervisor
// caches the connection, so a Chrome that restarts -- observed at
// NRestarts=230 from the stale SingletonLock -- would leave every browser tool
// dead until agentd itself was restarted. Both now surface as ordinary tool
// text the model can react to.
//
// The store is per-agent and sits under the agent's own state directory, so it
// falls inside roots.own and Read reaches spilled snapshots with no change to
// path confinement. The browser is shared, because there is one.
func attachBrowser(d *toolDeps, dir string, team *Supervisor) error {
	snaps, err := newSnapshotStore(dir)
	if err != nil {
		return err
	}
	d.snaps = snaps
	if team == nil {
		return fmt.Errorf("no supervisor to share a browser through")
	}
	d.chrome = func(ctx context.Context) (browser, error) { return team.Chrome(ctx) }
	return nil
}

// browserTools builds the browser surface, or nothing when the agent has no
// browser. Chrome is shared with a person watching the same screen, so an agent
// that was not given the browser must not be able to move it.
func browserTools(d toolDeps) ([]anthropic.BetaTool, error) {
	if !d.browser {
		return nil, nil
	}
	return buildTools(
		func() (anthropic.BetaTool, error) { return navigateTool(d) },
		func() (anthropic.BetaTool, error) { return snapshotTool(d) },
		func() (anthropic.BetaTool, error) { return clickTool(d) },
		func() (anthropic.BetaTool, error) { return fillTool(d) },
		func() (anthropic.BetaTool, error) { return pressKeyTool(d) },
		func() (anthropic.BetaTool, error) { return waitForTool(d) },
		func() (anthropic.BetaTool, error) { return dialogTool(d) },
	)
}

// usePage gets the shared browser and refuses early when a dialog holds it.
//
// Both failures come back as text for the caller to hand the model, never as a
// Go error: an error becomes an is_error result the model reads as a broken
// tool rather than as something it can fix. The dialog check is a local read
// with no CDP in it, so a blocked renderer costs nothing -- actually sending
// the command would park it until the deadline and burn the turn.
func usePage(ctx context.Context, d toolDeps) (browser, string) {
	b, why := reachChrome(ctx, d)
	if why != "" {
		return nil, why
	}
	if why := b.Blocked(); why != "" {
		return nil, why
	}
	return b, ""
}

// reachChrome resolves the shared browser without the dialog check, which is
// what handle_dialog needs: it is the one tool that must work while a dialog is
// open, since it is the only thing that can clear one.
func reachChrome(ctx context.Context, d toolDeps) (browser, string) {
	if d.chrome == nil {
		return nil, "this agent has no browser"
	}
	b, err := d.chrome(ctx)
	if err != nil {
		return nil, "chrome is not answering: " + err.Error()
	}
	return b, ""
}

// navigateTool drives the shared browser to a URL.
func navigateTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[navigateInput](
		"navigate_page", "Open a URL in the browser and report where it ended up.",
		func(ctx context.Context, in navigateInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := usePage(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			final, err := b.Navigate(ctx, in.URL)
			if err != nil {
				return toolText(fmt.Sprintf("could not open %s: %v", in.URL, err)), nil
			}
			return toolText(navigateResult(in.URL, final)), nil
		})
}

// navigateResult reports the landing URL, calling out a redirect explicitly so
// the model notices a consent wall or a login gate instead of assuming it is
// looking at the page it asked for.
func navigateResult(asked, final string) string {
	if final == "" || final == asked {
		return "opened " + asked + "\nTake a snapshot to see what is on the page."
	}
	return "opened " + asked + "\nredirected to " + final +
		"\nTake a snapshot to see what is on the page."
}

// snapshotTool reads the page into elements the model can act on.
func snapshotTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[snapshotInput](
		"take_snapshot",
		"Read the current page and list the elements you can act on with their uids.",
		func(ctx context.Context, in snapshotInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := usePage(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			snap, err := b.Take(ctx)
			if err != nil {
				return toolText(fmt.Sprintf("could not read the page: %v", err)), nil
			}
			return toolText(digestSnapshot(d, snap)), nil
		})
}

// digestSnapshot caps what the model sees and spills the rest to disk.
//
// A failure to spill is reported into the event log rather than swallowed: this
// is the only durable record that the digest is doing anything, and a digest
// that has quietly stopped working looks exactly like one that is working until
// the bill arrives.
func digestSnapshot(d toolDeps, snap *cdp.Snapshot) string {
	full := snap.Render()
	if d.snaps == nil {
		return capTextAt(full, snapshotInlineCap)
	}
	text, err := d.snaps.digest(full)
	if err != nil && d.log != nil {
		d.log.Append(Event{Type: "error",
			Message: "could not save the page snapshot: " + err.Error()})
	}
	return emptyToNote(text)
}

// emptyToNote keeps a blank page from reading to the model as a failed call.
func emptyToNote(s string) string {
	if strings.TrimSpace(s) == "" {
		return "the page has no readable content yet - it may still be loading"
	}
	return s
}
