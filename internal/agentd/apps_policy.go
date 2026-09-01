package agentd

import "errors"

// appsExecTool is the only meta-tool that can act.
//
// Not a shortlist: enable_multi_execute is not a choice between batched and
// single execution, it is the ONLY execution path (internal/composio's execConf
// says so, verified against the live API). The other four search, read schemas,
// wait, or start a connection -- and a connection still needs the person to
// finish an OAuth flow in their own browser, so none of them can act alone.
const appsExecTool = "COMPOSIO_MULTI_EXECUTE_TOOL"

// errUnreadableCall refuses a call whose actions cannot be identified.
//
// Worded for the model, because it is handed back as the tool result: it says
// what was wrong and what shape to use, so a legitimate malformed call can be
// retried. A refusal the model cannot act on becomes a retry loop.
var errUnreadableCall = errors.New(
	"this call could not be read, so nothing ran. Its tools argument must be a " +
		"list, and every entry an object with a non-empty tool_slug string.")

// slugsIn reports every app action a call is about to take.
//
// The action's identity is an ARGUMENT, not the tool name. One
// COMPOSIO_MULTI_EXECUTE_TOOL call carries {"tools":[{"tool_slug":..},..]} -- up
// to fifty of them, and the agent-facing skill actively tells agents to batch --
// so a single tool_use block legitimately mixes reads and sends. Anything
// reading a top-level tool_slug finds nothing and, if it takes that for safety,
// waves every send through.
//
// Unidentifiable is never safe. A batch this cannot read is one whose contents
// nothing has checked, so it is an error rather than an empty answer. The one
// type assertion below covers nil args, a missing tools and a tools that is not
// a list -- and nil args does reach here, because Execute only returns early
// when JSON fails to parse, and empty input parses to nothing at all.
func slugsIn(name string, args map[string]any) ([]string, error) {
	if name != appsExecTool {
		return nil, nil
	}
	raw, ok := args["tools"].([]any)
	if !ok {
		return nil, errUnreadableCall
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		slug, err := slugOf(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, slug)
	}
	return out, nil
}

// slugOf reads one batch entry's action name.
//
// The whole batch fails on a bad entry rather than that entry being skipped:
// running the readable half of a call the model wrote as a unit is a decision
// nobody made.
func slugOf(entry any) (string, error) {
	call, ok := entry.(map[string]any)
	if !ok {
		return "", errUnreadableCall
	}
	slug, ok := call["tool_slug"].(string)
	if !ok || slug == "" {
		return "", errUnreadableCall
	}
	return slug, nil
}
