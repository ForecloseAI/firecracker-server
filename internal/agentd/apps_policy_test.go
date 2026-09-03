package agentd

import (
	"fmt"
	"testing"
)

// batch builds the arguments of one execute call, the way the model sends them:
// through JSON, so the test sees the same []any / map[string]any the hook does
// rather than Go literals that happen to type-assert.
func batch(t *testing.T, body string) map[string]any {
	t.Helper()
	var args map[string]any
	decodeBody(t, body, &args)
	return args
}

// THE test for this PR. A batch mixes reads and sends freely -- the agent-facing
// skill tells agents to batch -- so every entry has to be reported, in order. A
// reader that stops at the first entry lets a send ride behind a fetch.
func TestEveryActionInABatchIsReported(t *testing.T) {
	args := batch(t, `{"tools":[
		{"tool_slug":"GMAIL_FETCH_EMAILS","arguments":{}},
		{"tool_slug":"GMAIL_SEND_EMAIL","arguments":{"to":"a@b.c"}}]}`)
	got, err := callsIn(appsExecTool, args)
	if err != nil {
		t.Fatalf("callsIn: %v", err)
	}
	if len(got) != 2 || got[0].Slug != "GMAIL_FETCH_EMAILS" || got[1].Slug != "GMAIL_SEND_EMAIL" {
		t.Errorf("got %v, want both actions in order", got)
	}
	// The arguments come with the action, because they are what a person needs
	// to answer: who the mail is going to, not merely that mail is going.
	if got[1].Args["to"] != "a@b.c" {
		t.Errorf("the send lost its arguments: %v", got[1].Args)
	}
}

// The provider takes up to fifty per call, so nothing may cap out earlier.
func TestAFullBatchIsReadWhole(t *testing.T) {
	body := `{"tools":[`
	for i := range 50 {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf(`{"tool_slug":"GMAIL_SEND_EMAIL_%d"}`, i)
	}
	got, err := callsIn(appsExecTool, batch(t, body+`]}`))
	if err != nil {
		t.Fatalf("callsIn: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("read %d of 50", len(got))
	}
}

// THE other test for this PR. Every shape that is not a list of objects each
// carrying a non-empty tool_slug must REFUSE, not answer "no actions here" --
// which the caller would read as nothing to ask about, and run the call.
func TestACallThatCannotBeReadIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"no arguments at all":     `null`,
		"empty object":            `{}`,
		"tools is a string":       `{"tools":"GMAIL_SEND_EMAIL"}`,
		"tools is an object":      `{"tools":{"tool_slug":"GMAIL_SEND_EMAIL"}}`,
		"tools is a number":       `{"tools":3}`,
		"tools is null":           `{"tools":null}`,
		"entry is a string":       `{"tools":["GMAIL_SEND_EMAIL"]}`,
		"entry has no slug":       `{"tools":[{"arguments":{}}]}`,
		"slug is empty":           `{"tools":[{"tool_slug":""}]}`,
		"slug is a number":        `{"tools":[{"tool_slug":7}]}`,
		"slug is null":            `{"tools":[{"tool_slug":null}]}`,
		"one bad entry of three":  `{"tools":[{"tool_slug":"A"},{"x":1},{"tool_slug":"C"}]}`,
		"slug at the wrong level": `{"tool_slug":"GMAIL_SEND_EMAIL"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := callsIn(appsExecTool, batch(t, body))
			if err == nil {
				t.Fatalf("read %v out of an unreadable call, so nothing would ask about it", got)
			}
			if got != nil {
				t.Errorf("returned %v alongside the refusal", got)
			}
		})
	}
}

// A batch with nothing in it runs nothing, so it has nothing to ask about. This
// is the one empty answer that is not a failure to read.
func TestAnEmptyBatchIsNotAnError(t *testing.T) {
	got, err := callsIn(appsExecTool, batch(t, `{"tools":[]}`))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

// The other four cannot act, so none of them carries an action to report.
func TestTheOtherMetaToolsCarryNoActions(t *testing.T) {
	for _, name := range appsMetaTools {
		if name == appsExecTool {
			continue
		}
		got, err := callsIn(name, batch(t, `{"toolkits":["gmail"]}`))
		if err != nil || got != nil {
			t.Errorf("%s reported %v, %v", name, got, err)
		}
	}
}
