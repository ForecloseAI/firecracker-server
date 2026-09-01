package chat

import (
	"testing"

	"cracked/internal/agentapi"
)

// connectAsk is a raised connect card as the guest would have logged it.
func connectAsk(url string) agentapi.Event {
	return agentapi.Event{
		ID: 7, Type: "question", Kind: askConnect, Agent: "boss",
		Question: "Connect your Gmail so I can check that thread",
		UI:       &agentapi.UI{Kind: askConnect, URL: url},
	}
}

// THE test for this file. A connect card is the first URL on a card the GUEST
// authors -- every other one, the handoff's included, is minted by the host. A
// card carries more trust than a link in a sentence, so a prompt-injected agent
// pointing one at a page that merely looks like Google must not get a button.
func TestAConnectCardOnlyGetsAButtonForAProviderURL(t *testing.T) {
	bad := []string{
		"https://composio.dev.evil.example/connect",
		"https://evilcomposio.dev/connect",
		"http://backend.composio.dev/connect",
		"javascript:alert(1)",
		"https://accounts.google.com/o/oauth2/auth",
		"",
	}
	for _, raw := range bad {
		m, ok := projectMessage(connectAsk(raw))
		if !ok {
			t.Fatalf("%q did not project at all", raw)
		}
		if m.URL != "" {
			t.Errorf("%q was rendered as a button", raw)
		}
		if m.Text == "" {
			t.Errorf("%q dropped the whole card rather than just the button", raw)
		}
	}
}

// The real thing does get one, on both the history path and the live feed --
// which is why this is set in projectMessage, the one mapper they share.
func TestAProviderConnectURLIsCarriedOntoTheCard(t *testing.T) {
	const url = "https://backend.composio.dev/connect/abc123"
	m, ok := projectMessage(connectAsk(url))
	if !ok {
		t.Fatal("a connect ask did not project")
	}
	if m.URL != url {
		t.Errorf("URL is %q", m.URL)
	}
	if m.UI == nil || m.UI.Kind != askConnect {
		t.Errorf("UI is %+v", m.UI)
	}
}

// A handoff's URL is still the host's to mint. If a guest could set one here it
// would be handing out links to a machine screen that this service is supposed
// to be the only issuer of.
func TestAGuestCannotSetAHandoffURL(t *testing.T) {
	ev := connectAsk("https://backend.composio.dev/connect/abc")
	ev.Kind = askHandoff
	ev.UI.Kind = askHandoff
	m, _ := projectMessage(ev)
	if m.URL != "" {
		t.Errorf("a guest-set handoff URL survived: %q", m.URL)
	}
}
