package agentd

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// attachBrowser gives an agent its snapshot store and the shared browser.
//
// It starts nothing. Resolving the server per call instead is the whole point:
// agentd deliberately does not order itself after chrome.service, so an agent
// built during boot would otherwise have no browser tools for its entire life,
// with a single line in its log to say so. And Chrome restarts on
// Restart=always -- observed at NRestarts=230 from a stale SingletonLock --
// which takes the server's connection to it along.
//
// The store is per-agent and sits under the agent's own state directory, so it
// falls inside roots.own and Read reaches spilled snapshots with no change to
// path confinement. The server is shared, because there is one browser.
func attachBrowser(d *toolDeps, dir string, team *Supervisor) error {
	snaps, err := newSnapshotStore(dir)
	if err != nil {
		return err
	}
	d.snaps = snaps
	if team == nil {
		return fmt.Errorf("no supervisor to share a browser through")
	}
	d.chrome = team.Browser()
	return nil
}

// browserTools asks the server what it offers, or returns nothing when the
// agent has no browser. Chrome is shared with a person watching the same
// screen, so an agent that was not given the browser must not be able to move
// it.
//
// The context is made here rather than threaded through Tools: this is a
// one-off at agent construction, and every caller of Tools would otherwise grow
// a parameter for a path five of the six shipped profiles never take.
func browserTools(d toolDeps) ([]anthropic.BetaTool, error) {
	if !d.browser || d.chrome == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserListTimeout)
	defer cancel()
	return d.chrome.Tools(ctx, d)
}

// emptyToNote keeps a blank page from reading to the model as a failed call.
func emptyToNote(s string) string {
	if strings.TrimSpace(s) == "" {
		return "the page has no readable content yet - it may still be loading"
	}
	return s
}
