package agentd

import (
	"context"
	"fmt"
	"strings"

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

// browserToolNames is the family this profile flag switches on. Kept beside the
// constructors so the two cannot drift apart.
var browserToolNames = []string{"navigate_page", "take_snapshot"}

// pageDriver is the slice of a browser these tools use.
//
// Declared here rather than taking *cdp.Browser so this package's tests can
// fake a page in memory: the WebSocket fake lives in package cdp, where the
// unexported message type it needs is visible, and standing up a socket to test
// a string renderer would be absurd. *cdp.Browser satisfies this as written.
type pageDriver interface {
	Navigate(ctx context.Context, url string) (string, error)
	Take(ctx context.Context) (*cdp.Snapshot, error)
}

// attachBrowser gives an agent the shared Chrome and its own snapshot store.
//
// The store is per-agent and sits under the agent's own state directory, so it
// falls inside roots.own and the Read tool reaches spilled snapshots with no
// change to path confinement. The browser is shared, because there is one.
func attachBrowser(d *toolDeps, dir string, team *Supervisor) error {
	snaps, err := newSnapshotStore(dir)
	if err != nil {
		return err
	}
	d.snaps = snaps
	if team == nil {
		return fmt.Errorf("no supervisor to share a browser through")
	}
	chrome, err := team.Chrome(context.Background())
	if err != nil {
		return err
	}
	d.chrome = chrome
	return nil
}

// browserTools builds the browser surface, or nothing when the agent has no
// browser. Chrome is shared with a person watching the same screen, so an agent
// that was not given the browser must not be able to move it.
func browserTools(d toolDeps) ([]anthropic.BetaTool, error) {
	if !d.browser || d.chrome == nil {
		return nil, nil
	}
	return buildTools(
		func() (anthropic.BetaTool, error) { return navigateTool(d.chrome) },
		func() (anthropic.BetaTool, error) { return snapshotTool(d) },
	)
}

// navigateTool drives the shared browser to a URL.
func navigateTool(chrome pageDriver) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[navigateInput](
		"navigate_page", "Open a URL in the browser and report where it ended up.",
		func(ctx context.Context, in navigateInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			final, err := chrome.Navigate(ctx, in.URL)
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
			snap, err := d.chrome.Take(ctx)
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
