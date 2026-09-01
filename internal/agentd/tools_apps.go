package agentd

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
)

// appsMetaTools are the session's own tools, named here because permitted()
// has to widen a profile's allow list to include them.
//
// Named rather than discovered, for the same reason browserAllowed is: a
// profile's tools: list is matched by name, and a surface that only exists
// after a successful dial would silently vanish from a profile whenever the
// session was unreachable at the moment that agent was built.
var appsMetaTools = []string{
	"COMPOSIO_SEARCH_TOOLS", "COMPOSIO_GET_TOOL_SCHEMAS", "COMPOSIO_MANAGE_CONNECTIONS",
	"COMPOSIO_WAIT_FOR_CONNECTIONS", appsExecTool,
}

// appsTools asks the session what it offers, or returns nothing when this
// machine has no session -- which is the ordinary state when the host has no
// integration provider configured, and is why the whole feature needs no flag.
//
// The context is made here rather than threaded through Tools, matching
// browserTools: this is a one-off at agent construction, and every caller of
// Tools would otherwise grow a parameter for it.
func appsTools(d toolDeps) ([]anthropic.BetaTool, error) {
	if d.apps == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), appsListTimeout)
	defer cancel()
	tools, err := d.apps.Tools(ctx, d)
	if err != nil {
		// Degrade to an agent with no connected apps, never to an agent that
		// refuses to start. This dial crosses the internet, so a provider having
		// a bad minute must not be able to take the boss down with it. The
		// failure is not cached: the next agent built tries again.
		logTo(d, "no connected apps: "+err.Error())
		return nil, nil
	}
	return tools, nil
}
