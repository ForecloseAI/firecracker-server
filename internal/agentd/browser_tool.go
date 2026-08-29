package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicmcp "github.com/anthropics/anthropic-sdk-go/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// snapshotToolName is the one tool whose result is digested before the model
// ever sees it.
const snapshotToolName = "take_snapshot"

// mcpSession is what a tool wrapper needs from whatever server owns it: a live
// session, a way to retire a dead one, and a name to blame when neither works.
//
// browserServer already had the first two written exactly this way, so the
// browser satisfies this with nothing but a label. The third is not decoration:
// without it an agent whose Notion call fails is told "the browser is not
// answering", and it acts on that by taking a snapshot.
type mcpSession interface {
	session(ctx context.Context) (*mcpsdk.ClientSession, error)
	drop(dead *mcpsdk.ClientSession)
	label() string
}

// mcpTool is one MCP tool as the model sees it, from the browser or from a
// server the person registered.
//
// Deliberately not mcp.NewBetaTool. That helper binds a tool to one session and
// drops most of the schema, and both are bugs we would otherwise ship: Tools()
// runs once per agent at construction, so a server that dies and respawns would
// leave every existing agent holding tools pointed at a corpse. This one
// resolves the session on every call.
type mcpTool struct {
	// name is what the MODEL calls it: bare for the browser, namespaced for a
	// registered server. wire is what the SERVER answers to.
	//
	// Two fields and not one. Sending the namespaced name back over the wire
	// would make every registered tool fail with "unknown tool" at CALL time --
	// after registration reported success, after the schema reached the model,
	// and visible nowhere but a transcript.
	name string
	wire string

	desc   string
	schema anthropic.BetaToolInputSchemaParam
	srv    mcpSession
	deps   toolDeps

	// callTimeout bounds one call. Zero for the browser, which rides the turn's
	// context exactly as it always has.
	callTimeout time.Duration
}

func (t *mcpTool) Name() string        { return t.name }
func (t *mcpTool) Description() string { return t.desc }

// InputSchema is what the model is told the arguments are.
func (t *mcpTool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.schema }

// Execute runs the tool and converts what came back.
//
// Never returns a Go error: the runner renders one as an is_error result the
// model reads as a broken tool rather than as something it can fix, and it
// cannot tell that apart from the page legitimately saying no.
func (t *mcpTool) Execute(ctx context.Context,
	input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var args map[string]any
	if len(input) > 0 && json.Unmarshal(input, &args) != nil {
		return textBlocks("could not read the arguments for " + t.name), nil
	}
	res, err := t.call(ctx, args)
	if err != nil {
		return textBlocks(err.Error()), nil
	}
	if res.IsError {
		return textBlocks(resultText(res)), nil
	}
	return t.blocks(res), nil
}

// call runs the tool on a live session, once more through a fresh server if the
// old one has gone.
//
// The server dying is not exotic: Chrome restarts on Restart=always and takes
// the server's connection to it along. Without the retry the model would keep
// calling into a corpse, because a dead transport and a refusal arrive as the
// same is_error text and it has no way to tell them apart.
func (t *mcpTool) call(ctx context.Context, args map[string]any) (*mcpsdk.CallToolResult, error) {
	ctx, cancel := t.bounded(ctx)
	defer cancel()
	sess, err := t.srv.session(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s is not answering: %w", t.srv.label(), err)
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.wire, Arguments: args})
	if err == nil {
		return res, nil
	}
	t.srv.drop(sess)
	return t.retry(ctx, args)
}

// bounded caps one call. CallTool has no timeout of its own and rides the turn's
// context, so a server that accepts a connection and then says nothing would
// hold the turn open until the person gave up. Zero leaves the browser where it
// has always been.
func (t *mcpTool) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if t.callTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, t.callTimeout)
}

// retry gets one more attempt on a freshly spawned server.
func (t *mcpTool) retry(ctx context.Context, args map[string]any) (*mcpsdk.CallToolResult, error) {
	sess, err := t.srv.session(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s stopped answering: %w", t.srv.label(), err)
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.wire, Arguments: args})
	if err != nil {
		t.srv.drop(sess)
		return nil, fmt.Errorf("%s stopped answering: %w", t.srv.label(), err)
	}
	return res, nil
}

// blocks converts the server's content, digesting a snapshot on the way past.
//
// StructuredContent is checked first because a third-party server may answer
// with only that: the spec allows it, chrome-devtools-mcp never does it, and
// without this the model would be handed an empty result and told nothing went
// wrong.
func (t *mcpTool) blocks(res *mcpsdk.CallToolResult) []anthropic.BetaToolResultBlockParamContentUnion {
	if len(res.Content) == 0 && res.StructuredContent != nil {
		return textBlocks(marshalOrNote(res.StructuredContent))
	}
	out := make([]anthropic.BetaToolResultBlockParamContentUnion, 0, len(res.Content))
	for _, c := range res.Content {
		block, err := anthropicmcp.ToBlock(c)
		if err != nil {
			return textBlocks(t.srv.label() + " sent something I cannot pass on: " + err.Error())
		}
		out = append(out, block)
	}
	if t.name == snapshotToolName {
		digestSnapshotBlocks(out, t.deps)
	}
	return out
}

// digestSnapshotBlocks spills a large snapshot to disk and leaves the model a
// capped view naming the path.
//
// This is the one piece of the hand-written browser that had to survive the
// switch to the MCP server. A content-rich page snapshots at 6-11k tokens and
// every tool result is re-sent on every later turn, so a handful of them come
// to dominate a conversation -- the TypeScript agent measured one session
// re-reading 119,598 cached tokens in a single turn before it grew an
// equivalent of this. A spill failure degrades the result rather than losing
// the page, and is reported into the event log, which is the only durable sign
// the digest is doing anything at all.
func digestSnapshotBlocks(blocks []anthropic.BetaToolResultBlockParamContentUnion, d toolDeps) {
	if d.snaps == nil {
		return
	}
	for _, b := range blocks {
		if b.OfText == nil {
			continue
		}
		text, err := d.snaps.digest(b.OfText.Text)
		if err != nil && d.log != nil {
			d.log.Append(Event{Type: "error",
				Message: "could not save the page snapshot: " + err.Error()})
		}
		b.OfText.Text = emptyToNote(text)
		return
	}
}

// wrapAll turns the browser's tools into the surface the model is offered.
// Bare names, and name == wire: renaming them would invalidate every beats
// entry and every profile's tools: list for no gain at all.
func wrapAll(tools []*mcpsdk.Tool, srv mcpSession, d toolDeps) []anthropic.BetaTool {
	out := make([]anthropic.BetaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, &mcpTool{
			name: t.Name, wire: t.Name, desc: t.Description, schema: schemaOf(t),
			srv: srv, deps: d,
		})
	}
	return out
}

// wrapSpecs turns one registered server's stored tools into the model's
// surface, under names namespaced to the server that advertised them.
//
// Built from specs rather than a live listing: an agent must start when a
// registered server is down, and these schemas were captured when the person
// registered it. It carries no toolDeps -- a registered tool has no per-agent
// state, so the wrappers are safe to build once and share.
func wrapSpecs(specs []mcpToolSpec, srv mcpSession, id string, timeout time.Duration) []anthropic.BetaTool {
	out := make([]anthropic.BetaTool, 0, len(specs))
	for _, s := range specs {
		name := modelName(id, s.Name)
		if !validToolName(name) {
			continue // refused at registration; belt and braces here
		}
		out = append(out, &mcpTool{
			name: name, wire: s.Name, desc: s.Desc, schema: schemaOfRaw(s.Schema),
			srv: srv, callTimeout: timeout,
		})
	}
	return out
}

// maxToolName is the Messages API limit on a tool name: 1-128 characters,
// letters, digits, underscores and hyphens.
const maxToolName = 128

var toolNameShape = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// modelName is the namespaced name a registered tool is offered under.
func modelName(serverID, tool string) string {
	return agentapi.MCPToolPrefix + serverID + "__" + tool
}

// validToolName reports whether the API will accept a name.
//
// Checked when the person registers, not when the model calls: the alternative
// is a 400 on their next turn naming a tool they have never heard of.
func validToolName(name string) bool {
	return name != "" && len(name) <= maxToolName && toolNameShape.MatchString(name)
}

// schemaOfRaw revives a stored schema, keeping every key the server advertised.
func schemaOfRaw(raw json.RawMessage) anthropic.BetaToolInputSchemaParam {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return anthropic.BetaToolInputSchemaParam{}
	}
	return anthropic.BetaToolInputSchema(m)
}

// marshalOrNote renders a structured-only result, or says why it could not.
func marshalOrNote(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return "this tool answered with something that could not be read"
	}
	return strings.TrimSpace(string(buf))
}

// schemaOf preserves the whole JSON schema the server advertised.
//
// NOT mcp.NewBetaTool's conversion. That round-trips into a struct with fields
// for only properties, required and type, and its ExtraFields map is tagged
// without the one the decoder needs -- so $defs, additionalProperties, anyOf
// and even a top-level description are dropped with nothing reporting it, and
// the model is left guessing arguments. BetaToolInputSchema is the SDK's own
// path that keeps them.
func schemaOf(t *mcpsdk.Tool) anthropic.BetaToolInputSchemaParam {
	if m, ok := t.InputSchema.(map[string]any); ok {
		return anthropic.BetaToolInputSchema(m)
	}
	return anthropic.BetaToolInputSchemaParam{}
}

// resultText joins whatever text an errored result carried.
func resultText(res *mcpsdk.CallToolResult) string {
	var out string
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok && tc.Text != "" {
			out += tc.Text + "\n"
		}
	}
	if out == "" {
		return "the browser tool failed and said nothing about why"
	}
	return out
}

// textBlocks is one text result, in the shape Execute must return.
func textBlocks(s string) []anthropic.BetaToolResultBlockParamContentUnion {
	return []anthropic.BetaToolResultBlockParamContentUnion{toolText(s)}
}
