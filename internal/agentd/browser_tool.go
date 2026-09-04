package agentd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicmcp "github.com/anthropics/anthropic-sdk-go/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// snapshotToolName is the one tool whose result is digested before the model
// ever sees it.
const snapshotToolName = "take_snapshot"

// mcpOwner is what a wrapped tool needs from whatever holds its server: a live
// session, and somewhere to report one that has died.
//
// An interface rather than *browserServer because two servers now ride this
// adapter -- the local Chrome one and the remote connected-apps one -- and the
// retry logic below is the same for both.
type mcpOwner interface {
	session(context.Context) (*mcpsdk.ClientSession, error)
	drop(*mcpsdk.ClientSession)
	// redact takes anything secret about the server out of an error before it
	// becomes a tool result the model reads and the event log keeps.
	redact(error) error
}

// beforeHook runs on the tool name and its parsed arguments before a call is
// made, and refuses it by returning an error. It is how the approval gate
// reaches a tool whose real identity is an argument rather than its name.
type beforeHook func(ctx context.Context, name string, args map[string]any) error

// The names a wrapped tool calls its server in an error the model reads.
const (
	browserNoun = "the browser"
	appsNoun    = "the connected app"
)

// mcpTool is one MCP tool as the model sees it.
//
// Deliberately not mcp.NewBetaTool. That helper binds a tool to one session and
// drops most of the schema, and both are bugs we would otherwise ship: Tools()
// runs once per agent at construction, so a server that dies and respawns would
// leave every existing agent holding tools pointed at a corpse. This one
// resolves the session on every call.
type mcpTool struct {
	name   string
	desc   string
	schema anthropic.BetaToolInputSchemaParam
	srv    mcpOwner
	// noun names the server in an error the model reads, so "the browser is not
	// answering" does not turn up when a connected app is what failed.
	noun   string
	before beforeHook
	deps   toolDeps
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
	// Before the call and after the parse: the hook decides on the ARGUMENTS,
	// which is where a meta-tool keeps the identity of what it is about to do.
	if t.before != nil {
		if err := t.before(ctx, t.name, args); err != nil {
			return textBlocks(err.Error()), nil
		}
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
	sess, err := t.srv.session(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s is not answering: %w", t.noun, err)
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.name, Arguments: args})
	if err == nil {
		return res, nil
	}
	t.srv.drop(sess)
	return t.retry(ctx, args)
}

// retry gets one more attempt on a freshly spawned server.
func (t *mcpTool) retry(ctx context.Context, args map[string]any) (*mcpsdk.CallToolResult, error) {
	sess, err := t.srv.session(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s stopped answering: %w", t.noun, err)
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.name, Arguments: args})
	if err != nil {
		t.srv.drop(sess)
		return nil, fmt.Errorf("%s stopped answering: %w", t.noun, t.srv.redact(err))
	}
	return res, nil
}

// blocks converts the server's content, digesting a snapshot on the way past.
func (t *mcpTool) blocks(res *mcpsdk.CallToolResult) []anthropic.BetaToolResultBlockParamContentUnion {
	out := make([]anthropic.BetaToolResultBlockParamContentUnion, 0, len(res.Content))
	for _, c := range res.Content {
		block, err := anthropicmcp.ToBlock(c)
		if err != nil {
			return textBlocks(t.noun + " sent something I cannot pass on: " + err.Error())
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
		if err != nil {
			logTo(d, "could not save the page snapshot: "+err.Error())
		}
		b.OfText.Text = emptyToNote(text)
		return
	}
}

// wrapAll turns a server's tools into the surface the model is offered. noun
// names it in errors, and before -- which may be nil -- gates each call.
func wrapAll(tools []*mcpsdk.Tool, srv mcpOwner, noun string,
	before beforeHook, d toolDeps) []anthropic.BetaTool {
	out := make([]anthropic.BetaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, &mcpTool{
			name: t.Name, desc: t.Description, schema: schemaOf(t),
			srv: srv, noun: noun, before: before, deps: d,
		})
	}
	return out
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
		return "the tool failed and said nothing about why"
	}
	return out
}

// textBlocks is one text result, in the shape Execute must return.
func textBlocks(s string) []anthropic.BetaToolResultBlockParamContentUnion {
	return []anthropic.BetaToolResultBlockParamContentUnion{toolText(s)}
}
