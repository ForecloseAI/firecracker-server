package composio

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// No key means no client, which is what turns the whole feature off without
// anybody having to remember a flag.
func TestNoKeyMeansNoClient(t *testing.T) {
	if New("", "") != nil {
		t.Fatal("a client was built with no key")
	}
}

// The session body is a set of security decisions, not defaults to inherit, and
// every one of these was verified against the live API. A prompt injection that
// could disconnect someone's apps; a remote shell the guest has no use for and
// which is ON unless the block is present; and the execute switch that has to be
// on or the whole surface is inert.
func TestSessionRequestPinsTheDangerousOptionsShut(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{
			"session_id": "sess_1", "mcp": map[string]string{"type": "http", "url": "https://x/mcp"}})
	}))
	defer srv.Close()

	if _, err := New("k", srv.URL).NewSession(context.Background(), "u", "https://back"); err != nil {
		t.Fatal(err)
	}
	manage, _ := got["manage_connections"].(map[string]any)
	if manage["enable_connection_removal"] != false {
		t.Error("a model could disconnect someone's apps")
	}
	if manage["callback_url"] != "https://back" {
		t.Errorf("callback_url is %v", manage["callback_url"])
	}
	// ON, and this is the one that reads backwards until you have seen the live
	// API: enable_multi_execute is not batched-versus-single, it is the ONLY
	// execute path. False makes the session unable to act at all.
	if ex, _ := got["execute"].(map[string]any); ex["enable_multi_execute"] != true {
		t.Error("the session was given no way to execute anything")
	}
	// Explicitly false, because OMITTING the block turns the workbench on and
	// brings a remote shell with it.
	if wb, ok := got["workbench"].(map[string]any); !ok || wb["enable"] != false {
		t.Error("a remote shell was left on")
	}
}

// The person's id travels as given. The machine id is the same UUID with its
// hyphens stripped, so sending the wrong one would isolate someone from their
// own connections without erroring.
func TestUserIDTravelsUnchanged(t *testing.T) {
	const sub = "3f8a1c92-5e4b-4d1a-9c77-0b2e5a6f8d31"
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("x-api-key"); key != "k" {
			t.Errorf("x-api-key is %q", key)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{
			"session_id": "s", "mcp": map[string]string{"url": "https://x/mcp"}})
	}))
	defer srv.Close()

	sess, err := New("k", srv.URL).NewSession(context.Background(), sub, "")
	if err != nil {
		t.Fatal(err)
	}
	if got["user_id"] != sub {
		t.Errorf("user_id is %v", got["user_id"])
	}
	if sess.ID != "s" || sess.URL != "https://x/mcp" {
		t.Errorf("session is %+v", sess)
	}
}

// A session with no endpoint is an error, not a session. Storing one would hand
// every machine a URL it can never dial and report nothing.
func TestASessionWithNoEndpointIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"session_id": "sess_1"})
	}))
	defer srv.Close()
	if _, err := New("k", srv.URL).NewSession(context.Background(), "u", ""); err == nil {
		t.Fatal("a session with no mcp url was accepted")
	}
}

// A refusal names the status, and never the key: this error travels into logs
// the operator console renders.
func TestAnErrorCarriesTheStatusAndNotTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := New("sk-secret-value", srv.URL).NewSession(context.Background(), "u", "")
	if err == nil {
		t.Fatal("a 403 was accepted")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error does not name the status: %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Errorf("the error leaked the key: %v", err)
	}
}

// THE test for the connections API. `user_id` singular is not rejected by the
// provider, it is IGNORED -- it answers with other people's accounts in the
// list. A caller that revoked what that returned would hand back strangers'
// grants, so the plural is asserted on the QUERY STRING, not on the result.
func TestConnectionsFiltersByUserIdsPlural(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"id": "ca_1", "status": "ACTIVE",
				"toolkit": map[string]string{"slug": "gmail"}}}})
	}))
	defer srv.Close()

	held, err := New("k", srv.URL).Connections(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "user_ids=user-1") {
		t.Errorf("query was %q, which must carry user_ids= and not user_id=", gotQuery)
	}
	if strings.Contains(strings.ReplaceAll(gotQuery, "user_ids", ""), "user_id") {
		t.Errorf("query %q uses the singular form, which the provider ignores", gotQuery)
	}
	want := []Connection{{ID: "ca_1", Toolkit: "gmail", Status: "ACTIVE"}}
	if len(held) != 1 || held[0] != want[0] {
		t.Errorf("got %+v", held)
	}
}

// A person with more accounts than fit in one page still has all of them
// revoked; stopping at page one would leave live grants behind.
func TestConnectionsFollowsTheCursor(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		body := map[string]any{"items": []any{
			map[string]any{"id": "ca_" + r.URL.Query().Get("cursor"), "status": "ACTIVE",
				"toolkit": map[string]string{"slug": "slack"}}}}
		if pages < 3 {
			body["next_cursor"] = "p" + string(rune('0'+pages))
		}
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	held, err := New("k", srv.URL).Connections(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 3 {
		t.Errorf("walked %d pages and collected %d accounts", pages, len(held))
	}
}

// A DELETE answers with no body at all. Decoding one is an EOF, which would
// report every successful revoke as a failure -- and account deletion refuses to
// proceed on a failed revoke, so that would make erasing an account impossible.
func TestDisconnectToleratesAnEmptyBody(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New("k", srv.URL).Disconnect(context.Background(), "ca_1"); err != nil {
		t.Fatalf("a 204 was read as a failure: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/connected_accounts/ca_1" {
		t.Errorf("sent %s %s", gotMethod, gotPath)
	}
}

// A toolkit's copy comes from the provider, so nothing about an app is written
// down here to go stale.
func TestToolkitReadsTheProvidersOwnCopy(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"slug":"gmail","name":"Gmail","meta":{
			"logo":"https://logos.composio.dev/api/gmail","description":"Google's email service"}}`))
	}))
	defer srv.Close()

	got, err := New("k", srv.URL).Toolkit(context.Background(), "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/toolkits/gmail" {
		t.Errorf("asked for %q", gotPath)
	}
	want := Toolkit{Slug: "gmail", Name: "Gmail",
		Logo: "https://logos.composio.dev/api/gmail", Description: "Google's email service"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

// THE test for this pair. toolkit_slug is SINGULAR here, and the plural form is
// not rejected -- it is ignored, answering with every config in the project. A
// caller that trusted the first row back would mint a link against whichever app
// happened to sort first, and connect the wrong one to somebody's account.
//
// It is the reverse of connected_accounts, where the plural user_ids is live and
// the singular is ignored. Neither is guessable, so both are pinned here.
func TestAuthConfigLooksUpByTheSingularSlug(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodGet:
			gotQuery = r.URL.RawQuery
			w.Write([]byte(`{"items":[{"id":"ac_1","toolkit":{"slug":"gmail"},
				"is_composio_managed":true,"status":"ENABLED"}]}`))
		default:
			json.NewEncoder(w).Encode(map[string]any{
				"redirect_url": "https://connect.composio.dev/link/lk_1",
				"expires_at":   "2099-01-01T00:00:00Z"})
		}
	}))
	defer srv.Close()

	if _, err := New("k", srv.URL).Link(context.Background(), "u", "gmail", "https://back"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "toolkit_slug=gmail") {
		t.Errorf("query was %q, which must carry the singular toolkit_slug", gotQuery)
	}
	if strings.Contains(gotQuery, "toolkit_slugs") {
		t.Errorf("query %q uses the plural, which the provider ignores", gotQuery)
	}
}

// A config the lookup did not find is created, because the provider will not
// infer one from the toolkit and the link call refuses to run without an id.
func TestAuthConfigIsCreatedWhenTheAppHasNoneYet(t *testing.T) {
	var created string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodGet:
			w.Write([]byte(`{"items":[]}`)) // nobody has connected this app yet
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodPost:
			var body struct {
				Toolkit struct {
					Slug string `json:"slug"`
				} `json:"toolkit"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			created = body.Toolkit.Slug
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"auth_config":{"id":"ac_new"}}`))
		default:
			var body linkReq
			json.NewDecoder(r.Body).Decode(&body)
			if body.AuthConfigID != "ac_new" {
				t.Errorf("the link used auth_config_id %q", body.AuthConfigID)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"redirect_url": "https://connect.composio.dev/link/lk_2",
				"expires_at":   "2099-01-01T00:00:00Z"})
		}
	}))
	defer srv.Close()

	got, err := New("k", srv.URL).Link(context.Background(), "u", "asana", "https://back")
	if err != nil {
		t.Fatal(err)
	}
	if created != "asana" {
		t.Errorf("created a config for %q", created)
	}
	if got.URL != "https://connect.composio.dev/link/lk_2" {
		t.Errorf("link url is %q", got.URL)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("the deadline was dropped, so nobody can tell a stale link from a fresh one")
	}
}

// A link with no url is an error, not a link. Handing one to a screen would
// render a button that goes nowhere.
func TestALinkWithNoURLIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth_configs" && r.Method == http.MethodGet {
			w.Write([]byte(`{"items":[{"id":"ac_1","toolkit":{"slug":"gmail"},
				"is_composio_managed":true,"status":"ENABLED"}]}`))
			return
		}
		w.Write([]byte(`{"expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	if _, err := New("k", srv.URL).Link(context.Background(), "u", "gmail", ""); err == nil {
		t.Fatal("a link with no url was accepted")
	}
}

// THE test for the classifier. Every one of these is a real slug with the real
// tags the provider returned on 2026-09-03, and each is here because something
// about it is a trap.
func TestCapabilityTakesTheMostConsequentialHint(t *testing.T) {
	for _, c := range []struct {
		slug, want string
		tags       []string
		why        string
	}{
		{"GMAIL_SEND_DRAFT", CapDelete, []string{"important", "openWorldHint", "destructiveHint", "updateHint"},
			"carries destructive AND update; the worse one has to win"},
		{"GMAIL_SEND_EMAIL", CapWrite, []string{"important", "openWorldHint", "createHint"}, ""},
		{"GMAIL_CREATE_EMAIL_DRAFT", CapWrite, []string{"important", "openWorldHint", "createHint"},
			"byte-identical tags to SEND_EMAIL: the provider cannot tell a draft from a send"},
		{"GMAIL_FETCH_EMAILS", CapRead, []string{"gmail", "readOnlyHint", "idempotentHint"}, ""},
		{"SLACK_FIND_CHANNELS", CapRead, []string{"readOnlyHint", "idempotentHint", "openWorldHint"},
			"read AND openWorld; openWorld must not drag a read into a write"},
		{"GOOGLECALENDAR_CALENDAR_LIST_INSERT", CapWrite, []string{"openWorldHint", "idempotentHint", "createHint"},
			"LIST is a noun here; only the tags decide"},
		{"GOOGLECALENDAR_CLEAR_CALENDAR", CapDelete, []string{"destructiveHint", "idempotentHint"}, ""},
		{"SOME_TOOL_SHIPPED_TOMORROW", CapWrite, []string{"gmail"},
			"no effect hint at all: a write, so it asks"},
		{"SOME_TOOL_WITH_NO_TAGS", CapWrite, nil, "nil tags must not panic and must not read as safe"},
	} {
		if got := capabilityOf(c.tags); got != c.want {
			t.Errorf("%s is %q, want %q%s", c.slug, got, c.want, note(c.why))
		}
	}
}

// note renders a fixture's reason, when it has one.
func note(why string) string {
	if why == "" {
		return ""
	}
	return " -- " + why
}

// Unfiltered on purpose: the rows carry their own tags, so there is no filter to
// be silently ignored. Asking for tags= and trusting the answer is what would
// call every tool read-only the day the parameter is renamed.
func TestCapabilitiesAsksForEveryToolAndClassifiesTheRows(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"items":[
			{"slug":"GMAIL_FETCH_EMAILS","tags":["readOnlyHint"],"toolkit":{"slug":"gmail"}},
			{"slug":"GMAIL_SEND_EMAIL","tags":["openWorldHint","createHint"],"toolkit":{"slug":"gmail"}},
			{"slug":"GMAIL_OLD_SEARCH","tags":["readOnlyHint"],"toolkit":{"slug":"gmail"},
				"is_deprecated":true},
			{"slug":"GMAIL_DELETE_MESSAGE","tags":["destructiveHint"],"toolkit":{"slug":"gmail"}}]}`))
	}))
	defer srv.Close()

	got, err := New("k", srv.URL).Capabilities(context.Background(), "gmail")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if strings.Contains(gotQuery, "tags=") {
		t.Errorf("query %q filters by tag; the rows are what decide", gotQuery)
	}
	if !strings.Contains(gotQuery, "toolkit_slug=gmail") {
		t.Errorf("query %q does not name the app", gotQuery)
	}
	// The deprecated row is deliberately absent rather than classified: absent
	// resolves to asking, and a withdrawn action that still reads as safe is one
	// nobody chose to keep running.
	want := map[string]string{
		"GMAIL_FETCH_EMAILS": CapRead, "GMAIL_SEND_EMAIL": CapWrite,
		"GMAIL_DELETE_MESSAGE": CapDelete,
	}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// THE test for the trap this API has already sprung twice -- user_id against
// user_ids, toolkit against toolkit_slug. A filter it does not recognise is
// IGNORED rather than refused, and the answer to "what can gmail do" quietly
// becomes the whole catalogue: classified, cached for an hour and pushed to a
// machine as the set of actions that may run unasked.
//
// The whole page is refused rather than filtered down to the rows that match,
// because those rows would then be cached as a complete answer for an app whose
// real tools never arrived.
func TestAPageAboutAnotherAppIsRefusedRatherThanFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[
			{"slug":"GMAIL_FETCH_EMAILS","tags":["readOnlyHint"],"toolkit":{"slug":"gmail"}},
			{"slug":"JIRA_DELETE_ISSUE","tags":["readOnlyHint"],"toolkit":{"slug":"jira"}}]}`))
	}))
	defer srv.Close()
	got, err := New("k", srv.URL).Capabilities(context.Background(), "gmail")
	if err == nil {
		t.Fatalf("kept %v from a page carrying another app's tools", got)
	}
	if !strings.Contains(err.Error(), "jira") {
		t.Errorf("the error does not name what came back instead: %v", err)
	}
}

// A full page is refused rather than kept: the tools it did not return would be
// absent from the map, and absent resolves to asking about everything.
func TestAFullPageOfCapabilitiesIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[` + strings.Repeat(
			`{"slug":"X","tags":["readOnlyHint"],"toolkit":{"slug":"outlook"}},`, toolPage-1) +
			`{"slug":"X","tags":["readOnlyHint"],"toolkit":{"slug":"outlook"}}]}`))
	}))
	defer srv.Close()
	if got, err := New("k", srv.URL).Capabilities(context.Background(), "outlook"); err == nil {
		t.Fatalf("kept %d off a full page", len(got))
	}
}

// THE test for the decode ceiling, and the reason the guard above is not enough
// on its own: its rows are 36 bytes, where a real unfiltered one carries a
// description and an input schema and runs past two kilobytes.
//
// A page just under the row guard is therefore about 1.1 MiB of body. Decoded
// under a 1 MiB ceiling that is not a short read but a total one -- io.LimitReader
// hands the decoder an early EOF, Decode returns "unexpected EOF" and NOTHING is
// kept, so the toolkit that outgrew the call reports an outage rather than the
// named paging error written for it. Never cached, so it re-fans-out forever.
func TestARealSizedPageIsDecodedRatherThanTruncated(t *testing.T) {
	row := `{"slug":"OUTLOOK_%d","tags":["readOnlyHint"],"toolkit":{"slug":"outlook"},` +
		`"description":"` + strings.Repeat("x", 2<<10) + `"},`
	var body strings.Builder
	body.WriteString(`{"items":[`)
	for i := range toolPage - 2 {
		body.WriteString(fmt.Sprintf(row, i))
	}
	body.WriteString(`{"slug":"OUTLOOK_LAST","tags":["destructiveHint"],` +
		`"toolkit":{"slug":"outlook"}}]}`)
	if body.Len() < 1<<20 {
		t.Fatalf("fixture is %d bytes, under the ceiling it exists to exceed", body.Len())
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body.String()))
	}))
	defer srv.Close()
	got, err := New("k", srv.URL).Capabilities(context.Background(), "outlook")
	if err != nil {
		t.Fatalf("a page under the row guard did not decode: %v", err)
	}
	if len(got) != toolPage-1 {
		t.Errorf("kept %d of %d rows", len(got), toolPage-1)
	}
	if got["OUTLOOK_LAST"] != CapDelete {
		t.Errorf("the last row of a big page was lost or misread: %q", got["OUTLOOK_LAST"])
	}
}

// The catalogue is walked to its end, and the rows come back in the provider's
// own usage order rather than re-sorted here -- a screen showing the first few
// of anything should show apps people actually connect.
func TestTheCatalogueIsWalkedToItsEnd(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("cursor") == "" {
			w.Write([]byte(`{"items":[{"slug":"gmail","name":"Gmail",
				"composio_managed_auth_schemes":["oauth2"],
				"meta":{"logo":"https://l/gmail","description":"Email",
					"tools_count":102,"categories":[{"id":"productivity","name":"Productivity"}]}}],
				"next_cursor":"c2"}`))
			return
		}
		w.Write([]byte(`{"items":[{"slug":"jira","name":"Jira",
			"composio_managed_auth_schemes":["oauth2"],"meta":{"tools_count":40}}]}`))
	}))
	defer srv.Close()

	got, err := New("k", srv.URL).Toolkits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Slug != "gmail" || got[1].Slug != "jira" {
		t.Fatalf("got %+v, want both pages in the order they arrived", got)
	}
	if got[0].Tools != 102 || len(got[0].Categories) != 1 ||
		got[0].Categories[0].ID != "productivity" {
		t.Errorf("the grouping and the size did not survive the decode: %+v", got[0])
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "sort_by=usage") {
		t.Errorf("queries were %q", queries)
	}
	if !strings.Contains(queries[1], "cursor=c2") {
		t.Errorf("the second page did not carry the cursor: %q", queries[1])
	}
}

// A cursor that never empties is a provider bug, and the pages that did arrive
// are not an answer: the caller caches what it is given, so a truncated
// catalogue would become an hour of apps nobody can find.
func TestACatalogueThatNeverEndsIsRefusedRatherThanTruncated(t *testing.T) {
	var pages atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		w.Write([]byte(`{"items":[{"slug":"gmail"}],"next_cursor":"always"}`))
	}))
	defer srv.Close()
	if got, err := New("k", srv.URL).Toolkits(context.Background()); err == nil {
		t.Fatalf("kept %d rows off a walk that never ended", len(got))
	}
	if n := pages.Load(); n != toolkitPages {
		t.Errorf("asked %d times, want the walk to stop at %d", n, toolkitPages)
	}
}

// Three ways an app cannot be offered, and they are three different things
// going wrong -- so each is pinned rather than left to whichever the filter
// happens to catch first.
func TestOnlyAnAppWeCanActuallyPutSomebodyThroughIsConnectable(t *testing.T) {
	for name, c := range map[string]struct {
		kit  Toolkit
		want bool
	}{
		"a managed app":            {Toolkit{ManagedAuth: true}, true},
		"nothing to authorise":     {Toolkit{ManagedAuth: true, NoAuth: true}, false},
		"no credentials of theirs": {Toolkit{}, false},
		"on its way out":           {Toolkit{ManagedAuth: true, Deprecated: true}, false},
	} {
		if got := c.kit.Connectable(); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// The deprecated block is ABSENT for a healthy app rather than false, so what
// marks one is the field being there at all.
func TestAWithdrawnAppIsRecognisedByTheBlockBeingPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"slug":"oldapp","name":"Old App",
			"composio_managed_auth_schemes":["oauth2"],
			"deprecated":{"version":"20250905_00"}}`))
	}))
	defer srv.Close()
	got, err := New("k", srv.URL).Toolkit(context.Background(), "oldapp")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deprecated || got.Connectable() {
		t.Errorf("got %+v, which is still on offer", got)
	}
}

// THE test for the config lookup, and it is about the one piece of provider
// state this project creates that outlives the request making it.
//
// A custom-typed config with no credentials behind it, or one somebody disabled,
// mints links that fail forever -- and a lookup taking the first row back would
// keep finding it. So both are asked for as filters AND re-checked on the rows,
// for the reason toolkit_slug is: an unrecognised filter here is ignored rather
// than refused.
func TestOnlyALiveManagedConfigIsReused(t *testing.T) {
	var gotQuery, created string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodGet:
			gotQuery = r.URL.RawQuery
			// What an ignored is_composio_managed filter looks like: rows the
			// project holds for this app that nobody can connect through.
			w.Write([]byte(`{"items":[
				{"id":"ac_custom","toolkit":{"slug":"gmail"},
					"is_composio_managed":false,"status":"ENABLED"},
				{"id":"ac_off","toolkit":{"slug":"gmail"},
					"is_composio_managed":true,"status":"DISABLED"}]}`))
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodPost:
			var body struct {
				AuthConfig struct {
					Type string `json:"type"`
				} `json:"auth_config"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			created = body.AuthConfig.Type
			w.Write([]byte(`{"auth_config":{"id":"ac_new"}}`))
		default:
			var body struct {
				AuthConfigID string `json:"auth_config_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.AuthConfigID != "ac_new" {
				t.Errorf("minted against %q, a config nobody can connect through",
					body.AuthConfigID)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"redirect_url": "https://connect.composio.dev/link/lk_1",
				"expires_at":   "2099-01-01T00:00:00Z"})
		}
	}))
	defer srv.Close()

	if _, err := New("k", srv.URL).Link(context.Background(), "u", "gmail", "https://back"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "is_composio_managed=true") {
		t.Errorf("the lookup did not ask for managed configs: %q", gotQuery)
	}
	// Sent rather than left to default. It decides whether the provider brings
	// credentials or expects ours, it is now reached for any app in the
	// catalogue, and what it makes sticks for the life of the project.
	if created != "use_composio_managed_auth" {
		t.Errorf("created a %q config", created)
	}
}

// The groupings are the provider's, so nothing about them is written down here
// either -- what a category is called is copy, the same as an app's blurb.
func TestCategoriesComeBackAsTheProviderNamesThem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/toolkits/categories" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		w.Write([]byte(`{"items":[{"id":"productivity","name":"Productivity"},
			{"id":"crm","name":"CRM"}]}`))
	}))
	defer srv.Close()
	got, err := New("k", srv.URL).Categories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Category{{ID: "productivity", Name: "Productivity"}, {ID: "crm", Name: "CRM"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}
