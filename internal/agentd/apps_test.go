package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeApps stands a real MCP server up in process, so these tests exercise the
// same handshake, tools/list and tools/call the guest will.
func fakeApps(t *testing.T, tools ...*mcpsdk.Tool) *appsServer {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake-apps", Version: "1"}, nil)
	for _, tool := range tools {
		srv.AddTool(tool, cannedResult(tool))
	}
	a := newAppsServer(agentapi.Apps{SessionURL: "https://backend.composio.dev/mcp/test"})
	a.dial = func(ctx context.Context, _ string) (*mcpsdk.ClientSession, error) {
		return connectInMemory(ctx, srv)
	}
	return a
}

// A machine with no session offers no tools, and that is not an error: it is
// every VM until the host pushes one, and it is what keeps this feature off
// without a flag of its own.
func TestNoSessionMeansNoTools(t *testing.T) {
	tools, err := newAppsServer(agentapi.Apps{SessionURL: ""}).Tools(context.Background(), toolDeps{})
	if err != nil {
		t.Fatalf("an unconfigured machine errored: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("%d tools were offered", len(tools))
	}
}

// The session's surface reaches the model unfiltered: it was configured
// host-side to advertise exactly what we want, so a second allow list here would
// only be a place for the two to disagree.
func TestSessionToolsReachTheModel(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"),
		namedTool("COMPOSIO_MULTI_EXECUTE_TOOL", "done"))
	tools, err := a.Tools(context.Background(), toolDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools", len(tools))
	}
}

// THE test for this file. A wrapped tool closes over ONE agent's deps -- its
// gate, its log -- so a surface built for the first agent and handed to the
// second would raise that agent's approvals in someone else's transcript. The
// listing is cached; the wrapping must not be.
func TestToolsAreWrappedPerAgentNotCached(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_MULTI_EXECUTE_TOOL", "done"))
	first, err := a.Tools(context.Background(), toolDeps{self: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Tools(context.Background(), toolDeps{self: "cody"})
	if err != nil {
		t.Fatal(err)
	}
	if first[0] == second[0] {
		t.Fatal("two agents were handed the same wrapped tool")
	}
	if got := second[0].(*mcpTool).deps.self; got != "cody" {
		t.Errorf("the second agent's tool belongs to %q", got)
	}
}

// Pointing the machine at a new session drops what was held for the old one, so
// a re-push after a host restart cannot leave agents dialling a dead endpoint.
func TestSetURLDropsTheOldSession(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"))
	if _, err := a.Tools(context.Background(), toolDeps{}); err != nil {
		t.Fatal(err)
	}
	a.SetURL("https://backend.composio.dev/mcp/other")
	if a.listed != nil || a.sess != nil {
		t.Error("the old session survived a repoint")
	}
	if a.Current() != "https://backend.composio.dev/mcp/other" {
		t.Errorf("Current is %q", a.Current())
	}
}

// Re-pushing the same session must not throw away a live connection and the
// tools/list it already paid for. The host pushes on every boot.
func TestSetURLToTheSameSessionKeepsIt(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"))
	if _, err := a.Tools(context.Background(), toolDeps{}); err != nil {
		t.Fatal(err)
	}
	a.SetURL(a.Current())
	if a.listed == nil || a.sess == nil {
		t.Error("an unchanged push dropped the session")
	}
}

// A tools/list can still be in flight after the host repoints the machine. Its
// result belongs to the captured URL and must not repopulate the new session's
// cache or put the new session into cooldown on failure.
func TestARepointDiscardsStaleListingState(t *testing.T) {
	const oldURL = "https://backend.composio.dev/mcp/old"
	const newURL = "https://backend.composio.dev/mcp/new"
	a := newAppsServer(agentapi.Apps{SessionURL: oldURL})
	a.SetURL(newURL)

	if a.store(oldURL, []*mcpsdk.Tool{namedTool("STALE", "stale")}) {
		t.Fatal("a listing from the old session was accepted")
	}
	if a.noteFailure(oldURL) {
		t.Fatal("a failure from the old session was accepted")
	}
	if a.listed != nil {
		t.Fatal("the old session repopulated the listing cache")
	}
	if !a.failedAt.IsZero() {
		t.Fatal("the old session put the new session into cooldown")
	}
}

// gated stands a session up whose provider calls only `reads` safe.
func gated(t *testing.T, reads ...string) (anthropic.BetaTool, *Gate) {
	t.Helper()
	a := fakeApps(t, namedTool("COMPOSIO_MULTI_EXECUTE_TOOL", "sent"))
	a.SetReadOnly(reads)
	g := NewGate(mustLog(t), NewInteractions(), t.TempDir())
	tools, err := a.Tools(context.Background(), toolDeps{gate: g})
	if err != nil {
		t.Fatal(err)
	}
	return tools[0], g
}

// answered runs one batch, settling every card with d, and reports what the
// provider said and what the person was shown. Check parks the caller, so the
// call runs in the background and the answers come from here.
func answered(t *testing.T, tool anthropic.BetaTool, g *Gate, d Decision, body string) (string, []Event) {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		out, _ := tool.Execute(context.Background(), json.RawMessage(body))
		done <- blockText(out) // Execute never returns a Go error; see its doc
	}()
	// Ticked, not spun: cards() parses the log off disk on every turn.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	fail := time.After(5 * time.Second)
	for {
		select {
		case got := <-done:
			return got, cards(t, g)
		case <-fail:
			t.Fatal("a card was raised that nothing answered")
		case <-tick.C:
			for _, e := range cards(t, g) {
				g.Resolve(e.ApprovalID, d) // false when already answered
			}
		}
	}
}

// cards is every approval this agent has been shown.
func cards(t *testing.T, g *Gate) []Event {
	t.Helper()
	events, err := g.log.ReadAll()
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	return slices.DeleteFunc(events, func(e Event) bool { return e.Type != "approval_required" })
}

// THE test for this file. A refused send must not reach the provider at all. The
// fake answers "sent", so the absence of that word is the proof.
func TestARefusedSendNeverReachesTheProvider(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS")
	got, shown := answered(t, tool, g, Decision{Decision: "deny", Reason: "not that one"},
		`{"tools":[{"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"dave@example.com"}}]}`)
	if strings.Contains(got, "sent") {
		t.Fatalf("the send reached the provider anyway: %q", got)
	}
	if !strings.Contains(got, "declined") {
		t.Errorf("the model was told %q, which does not read as a refusal", got)
	}
	if len(shown) != 1 {
		t.Fatalf("%d cards, want exactly one", len(shown))
	}
}

// And the other half: approving one lets it through, or the gate is just a wall.
func TestAnApprovedSendReachesTheProvider(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS")
	got, shown := answered(t, tool, g, Decision{Decision: "allow"},
		`{"tools":[{"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"dave@example.com"}}]}`)
	if !strings.Contains(got, "sent") {
		t.Errorf("an approved send did not reach the provider: %q", got)
	}
	if len(shown) != 1 {
		t.Errorf("%d cards, want exactly one", len(shown))
	}
}

// A read the provider calls read-only runs silently -- the half that makes the
// other half worth reading, since a gate that asks about everything is ignored.
func TestAReadTheProviderCallsSafeAsksNobody(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS", "SLACK_FIND_CHANNELS")
	got, shown := answered(t, tool, g, Decision{Decision: "deny"},
		`{"tools":[{"tool_slug":"GMAIL_FETCH_EMAILS","arguments":{}},
		           {"tool_slug":"SLACK_FIND_CHANNELS","arguments":{}}]}`)
	if len(shown) != 0 {
		t.Fatalf("reads raised %d cards", len(shown))
	}
	if !strings.Contains(got, "sent") {
		t.Errorf("the reads did not reach the provider: %q", got)
	}
}

// A batch mixes freely, so the gate must pick the send out and ask about it.
func TestAMixedBatchAsksOnlyAboutTheSend(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS")
	_, shown := answered(t, tool, g, Decision{Decision: "allow"},
		`{"tools":[{"tool_slug":"GMAIL_FETCH_EMAILS","arguments":{}},
		           {"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"dave@example.com"}}]}`)
	if len(shown) != 1 {
		t.Fatalf("%d cards, want one -- only the send needed a person", len(shown))
	}
	// Keyed on the ACTION, not the meta-tool. See permit for why that matters.
	if shown[0].Tool != "GMAIL_SEND_EMAIL" {
		t.Errorf("the card is about %q", shown[0].Tool)
	}
	// And it names who the mail is going to.
	if !strings.Contains(shown[0].Preview, "dave@example.com") {
		t.Errorf("the card does not say who it is to: %q", shown[0].Preview)
	}
}

// With no set at all -- a push that never landed, a provider having a bad day --
// every action asks.
func TestWithNoReadOnlySetEvenAReadAsks(t *testing.T) {
	tool, g := gated(t)
	_, shown := answered(t, tool, g, Decision{Decision: "allow"},
		`{"tools":[{"tool_slug":"GMAIL_FETCH_EMAILS","arguments":{}}]}`)
	if len(shown) != 1 {
		t.Errorf("%d cards for a read with no set; absent must mean ask", len(shown))
	}
}

// A batch nothing can read is refused without asking and without running: there
// is nothing to put on a card.
func TestAnUnreadableBatchIsRefusedWithoutAsking(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS")
	got, shown := answered(t, tool, g, Decision{Decision: "allow"}, `{"tools":"GMAIL_SEND_EMAIL"}`)
	if strings.Contains(got, "sent") {
		t.Fatalf("an unreadable batch reached the provider: %q", got)
	}
	if len(shown) != 0 {
		t.Errorf("%d cards for a batch nothing could read", len(shown))
	}
}

// A dial that failed is remembered briefly. Building an agent is in the path of
// a person's message, so without this a provider having a bad ten minutes would
// add the full dial timeout to every message sent to an evicted agent.
func TestAFailedDialIsNotRetriedImmediately(t *testing.T) {
	a := newAppsServer(agentapi.Apps{SessionURL: "https://backend.composio.dev/mcp/test"})
	dials := 0
	a.dial = func(context.Context, string) (*mcpsdk.ClientSession, error) {
		dials++
		return nil, errNotDialable // any failure will do
	}
	for range 3 {
		if _, err := a.Tools(context.Background(), toolDeps{}); err == nil {
			t.Fatal("a broken session reported tools")
		}
	}
	if dials != 1 {
		t.Errorf("dialled %d times during the cooldown", dials)
	}
}

// The cooldown is not permanent: a provider that comes back is picked up.
func TestTheCooldownExpires(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"))
	a.failedAt = time.Now().Add(-appsRetryAfter - time.Second)
	tools, err := a.Tools(context.Background(), toolDeps{})
	if err != nil {
		t.Fatalf("a recovered session stayed shut: %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("got %d tools", len(tools))
	}
	if !a.failedAt.IsZero() {
		t.Error("a success left the failure marker set")
	}
}

// THE test for this file's security property. The session URL is a bearer
// credential for everything the person has connected, and transport errors wrap
// url.Error, which embeds the whole request URL. That string becomes a tool
// result the model reads and an entry in the event log -- so a prompt injection
// could read the capability straight out of its own context.
func TestTheSessionURLNeverTravelsInAnError(t *testing.T) {
	const url = "https://backend.composio.dev/mcp/sess_SECRET_CAPABILITY"
	a := newAppsServer(agentapi.Apps{SessionURL: url})
	a.dial = func(_ context.Context, endpoint string) (*mcpsdk.ClientSession, error) {
		// Shaped like what the transport really returns.
		return nil, fmt.Errorf(`Post %q: dial tcp: lookup failed`, endpoint)
	}

	_, err := a.Tools(context.Background(), toolDeps{})
	if err == nil {
		t.Fatal("a broken dial reported tools")
	}
	if strings.Contains(err.Error(), "sess_SECRET_CAPABILITY") {
		t.Errorf("the session url leaked into a Tools error: %v", err)
	}
	if !strings.Contains(err.Error(), sessionPlaceholder) {
		t.Errorf("the error lost the fact that it was about the session: %v", err)
	}

	// The same on the path that reaches the model: session() feeds mcpTool.call,
	// whose error becomes the tool result.
	a.failedAt = time.Time{}
	if _, err := a.session(context.Background()); strings.Contains(err.Error(), "sess_SECRET_CAPABILITY") {
		t.Errorf("the session url leaked into a call error: %v", err)
	}
	if got := a.redact(fmt.Errorf("wrapped: %s", url)); strings.Contains(got.Error(), "sess_SECRET") {
		t.Errorf("redact let it through: %v", got)
	}

	// The case a plain substring match misses: the client normalised what we
	// dialled -- here a trailing slash -- so the error carries a URL that is not
	// the string we stored. Walking the chain for the *url.Error's own URL is
	// what catches it.
	normalised := &neturl.Error{Op: "Post", URL: url + "/", Err: errors.New("dial tcp")}
	if got := a.redact(normalised); strings.Contains(got.Error(), "sess_SECRET_CAPABILITY") {
		t.Errorf("a normalised url leaked: %v", got)
	}
}

// blockText joins the text a tool result carried.
func blockText(blocks []anthropic.BetaToolResultBlockParamContentUnion) string {
	var out strings.Builder
	for _, b := range blocks {
		if b.OfText != nil {
			out.WriteString(b.OfText.Text)
		}
	}
	return out.String()
}

// THE test for the locking. A cold dial crosses the internet and can hang for
// the full timeout; while it does, every other caller must still be served.
// Holding the state lock across it -- which is what browserServer does, safely,
// because its dial is a local exec -- blocked a live agent's tool call, the
// error path reporting the failure, and the host's own PUT /apps.
func TestADialInFlightBlocksNobody(t *testing.T) {
	a := newAppsServer(agentapi.Apps{SessionURL: "https://backend.composio.dev/mcp/test"})
	dialing, release := make(chan struct{}), make(chan struct{})
	a.dial = func(ctx context.Context, _ string) (*mcpsdk.ClientSession, error) {
		close(dialing)
		<-release
		return nil, errNoSession
	}
	go a.Tools(context.Background(), toolDeps{})
	<-dialing // a dial is now in flight and will not finish until we say so

	done := make(chan string, 3)
	go func() { a.SetURL("https://backend.composio.dev/mcp/other"); done <- "SetURL" }()
	go func() { a.redact(errNoSession); done <- "redact" }()
	go func() { a.Current(); done <- "Current" }()
	for range 3 {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("a caller was blocked behind the dial")
		}
	}
	close(release)
}

// Redaction must not destroy the cause. The case most worth classifying
// downstream -- a timeout on a URL we had to redact -- is exactly the case where
// the message changes, so errors.New went false precisely when it mattered.
func TestRedactionKeepsTheCause(t *testing.T) {
	const url = "https://backend.composio.dev/mcp/sess_SECRET"
	wrapped := fmt.Errorf("Post %q: %w", url, context.DeadlineExceeded)

	got := redactURL(wrapped, url)
	if strings.Contains(got.Error(), "sess_SECRET") {
		t.Fatalf("the session url survived: %v", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Error("a timeout stopped looking like a timeout once redacted")
	}
	// An error with nothing to redact is handed back untouched, chain and all.
	plain := fmt.Errorf("dial: %w", context.Canceled)
	if got := redactURL(plain, url); !errors.Is(got, context.Canceled) {
		t.Error("an unredacted error lost its cause")
	}
}

// The same action twice in one batch is two actions. A map keyed on slug would
// collapse them, so one card would be answered and the provider would run both
// sends -- to different people.
func TestTheSameActionTwiceIsAskedAboutTwice(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS")
	_, shown := answered(t, tool, g, Decision{Decision: "allow"},
		`{"tools":[{"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"dave@example.com"}},
		           {"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"someone@else.com"}}]}`)
	if len(shown) != 2 {
		t.Fatalf("%d cards for two sends", len(shown))
	}
	if shown[0].Preview == shown[1].Preview {
		t.Error("both cards describe the same send, so one recipient was lost")
	}
}

// A refusal takes the whole batch with it, reads included. The provider runs a
// batch as one request, so there is no half of it to run -- and the safe
// direction is that nothing does.
func TestARefusalAbortsTheWholeBatch(t *testing.T) {
	tool, g := gated(t, "GMAIL_FETCH_EMAILS")
	got, _ := answered(t, tool, g, Decision{Decision: "deny"},
		`{"tools":[{"tool_slug":"GMAIL_FETCH_EMAILS","arguments":{}},
		           {"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"dave@example.com"}}]}`)
	if strings.Contains(got, "sent") {
		t.Error("the batch reached the provider after one of its actions was refused")
	}
}

// An action with no arguments adds no detail line. The host renders the slug as
// the card's title, so a preview repeating it would spend the one line a person
// reads on something already above it.
func TestAnActionWithNoArgumentsHasNoPreview(t *testing.T) {
	if got := previewOf(appCall{Slug: "GMAIL_ARCHIVE_ALL"}); got != "" {
		t.Errorf("preview is %q, which the card already says as its title", got)
	}
	if got := previewOf(appCall{Slug: "X", Args: map[string]any{"to": "d@e.com"}}); got != `{"to":"d@e.com"}` {
		t.Errorf("preview is %q", got)
	}
}

// A long argument is cut without splitting a character, and says it was cut.
func TestALongArgumentIsClippedOnARuneBoundary(t *testing.T) {
	got := previewOf(appCall{Slug: "X", Args: map[string]any{"body": strings.Repeat("é", 400)}})
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("a long body was not clipped: %d chars", len([]rune(got)))
	}
	if strings.ContainsRune(got, '�') {
		t.Error("the cut landed inside a character")
	}
}
