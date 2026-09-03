package agentd

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// previewCap bounds what one card carries, in runes.
//
// Nothing downstream truncates: the app renders the preview in a 270pt column
// and the web page in a pre-wrap div, both of which simply grow. This is what
// stops a mail body becoming a card nobody scrolls to the buttons of.
const previewCap = 240

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
//
// "Nothing ran" is a claim about the caller, true only while the hook hands this
// back unwrapped. Do not fmt.Errorf("...: %w", err) it.
var errUnreadableCall = errors.New(
	"this call could not be read, so nothing ran. Its tools argument must be a " +
		"list, and every entry an object with a non-empty tool_slug string.")

// appCall is one action inside a batch, with the arguments it will run with.
// The arguments are what a person needs to see to answer -- who the mail is
// going to, which channel is being posted in.
type appCall struct {
	Slug string
	Args map[string]any
}

// callsIn reports every app action a call is about to take.
//
// The action's identity is an ARGUMENT, not the tool name. One
// COMPOSIO_MULTI_EXECUTE_TOOL call carries {"tools":[{"tool_slug":..},..]} -- up
// to fifty of them, and the agent-facing skill actively tells agents to batch --
// so a single tool_use block legitimately mixes reads and sends. Anything
// reading a top-level tool_slug finds nothing and, if it takes that for safety,
// waves every send through.
//
// Unidentifiable is never safe. A batch this cannot read is one whose contents
// nothing has checked, so it is an error rather than an empty answer. Nil args
// does reach the one assertion below: Execute only returns early when JSON fails
// to parse, and empty input parses to nothing at all.
func callsIn(name string, args map[string]any) ([]appCall, error) {
	if name != appsExecTool {
		return nil, nil
	}
	raw, ok := args["tools"].([]any)
	if !ok {
		return nil, errUnreadableCall
	}
	out := make([]appCall, 0, len(raw))
	for _, entry := range raw {
		call, ok := callOf(entry)
		if !ok {
			return nil, errUnreadableCall
		}
		out = append(out, call)
	}
	return out, nil
}

// callOf reads one batch entry: what it will do, and what with.
//
// The whole batch fails on a bad entry rather than that entry being skipped:
// running the readable half of a call the model wrote as a unit is a decision
// nobody made.
func callOf(entry any) (appCall, bool) {
	// Refused here rather than left to the lookup below, which would also miss:
	// indexing a nil map is legal and yields nothing. Belt and braces, on purpose
	// -- what refuses a malformed entry should not rest on that.
	held, ok := entry.(map[string]any)
	if !ok {
		return appCall{}, false
	}
	slug, ok := held["tool_slug"].(string)
	if !ok || slug == "" {
		return appCall{}, false
	}
	// Absent or malformed arguments are not a refusal: they cost a card that
	// names the action with nothing under it, and plenty of actions take none.
	args, _ := held["arguments"].(map[string]any)
	return appCall{Slug: slug, Args: args}, true
}

// previewOf is what a person reads under the question: the arguments, and NOT
// the slug -- both surfaces already title the card "Allow GMAIL_SEND_EMAIL?", so
// repeating it spends the one line they read. Empty when there is nothing to add.
//
// Rendered as the provider gave them rather than picked over by field name: 910
// tools whose argument names are theirs to change, so a card showing only the
// fields we recognised would show nothing at all for a tool we did not.
func previewOf(c appCall) string {
	if len(c.Args) == 0 {
		return ""
	}
	// Cannot fail: Args came out of json.Unmarshal, so it holds only what json
	// can put back.
	return clip(marshalPreview(c.Args), previewCap)
}

// marshalPreview keeps destination-like arguments at the front of the bounded
// preview. encoding/json sorts map keys, which used to let a long body hide the
// recipient or channel that a person most needs when deciding whether to act.
func marshalPreview(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		pi, pj := previewPriority(keys[i]), previewPriority(keys[j])
		if pi != pj {
			return pi < pj
		}
		return keys[i] < keys[j]
	})

	var out strings.Builder
	out.WriteByte('{')
	for i, key := range keys {
		if i != 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		value, _ := json.Marshal(args[key])
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.String()
}

func previewPriority(key string) int {
	key = strings.ToLower(key)
	for _, part := range []string{"recipient", "channel", "destination", "address"} {
		if key == part || strings.Contains(key, part) {
			return 0
		}
	}
	if key == "to" || strings.HasSuffix(key, "_to") {
		return 0
	}
	return 1
}

// clip shortens to n runes, saying so rather than trailing off. Ranging a string
// yields the byte offset of each rune start, so the cut cannot halve a character
// and leave a replacement glyph over somebody's name -- and it does that without
// building a []rune of a whole mail body to keep 240 characters of it.
func clip(s string, n int) string {
	if len(s) <= n { // n bytes or fewer is n runes or fewer
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "... (truncated)"
		}
		count++
	}
	return s
}
