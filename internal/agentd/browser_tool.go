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

// mcpTool is one chrome-devtools-mcp tool as the model sees it.
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
	srv    *browserServer
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
		return nil, fmt.Errorf("the browser is not answering: %w", err)
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
		return nil, fmt.Errorf("the browser stopped answering: %w", err)
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.name, Arguments: args})
	if err != nil {
		t.srv.drop(sess)
		return nil, fmt.Errorf("the browser stopped answering: %w", err)
	}
	return res, nil
}

// blocks converts the server's content, digesting a snapshot on the way past.
func (t *mcpTool) blocks(res *mcpsdk.CallToolResult) []anthropic.BetaToolResultBlockParamContentUnion {
	out := make([]anthropic.BetaToolResultBlockParamContentUnion, 0, len(res.Content))
	for _, c := range res.Content {
		block, err := anthropicmcp.ToBlock(c)
		if err != nil {
			return textBlocks("the browser sent something I cannot pass on: " + err.Error())
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

// wrapAll turns the server's tools into the surface the model is offered.
func wrapAll(tools []*mcpsdk.Tool, srv *browserServer, d toolDeps) []anthropic.BetaTool {
	out := make([]anthropic.BetaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, &mcpTool{
			name: t.Name, desc: t.Description, schema: schemaOf(t), srv: srv, deps: d,
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
		return "the browser tool failed and said nothing about why"
	}
	return out
}

// textBlocks is one text result, in the shape Execute must return.
func textBlocks(s string) []anthropic.BetaToolResultBlockParamContentUnion {
	return []anthropic.BetaToolResultBlockParamContentUnion{toolText(s)}
}
