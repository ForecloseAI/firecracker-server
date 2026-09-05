package chat

import (
	"net/url"
	"strings"
	"testing"
)

// mustURL parses a URL a test wrote by hand.
func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// validConfig is the smallest environment the service accepts.
func validConfig() Config {
	return Config{
		Origin: "https://chat.example.com", VNCOrigin: "https://vnc.example.com",
		Token: "t", SupabaseURL: "https://p.supabase.co", AppsAddr: "0.0.0.0:8092",
		OpenRouterKey: "sk-or-x", OpenRouterUpstream: mustURL("https://openrouter.ai/api"),
	}
}

// The guest image no longer carries a model credential, so a chat host without
// one is every machine on the fleet unable to take a turn. That has to fail at
// startup, by name, rather than one agent at a time.
func TestTheModelKeyIsRequired(t *testing.T) {
	c := validConfig()
	if err := c.validate(); err != nil {
		t.Fatalf("a complete config was refused: %v", err)
	}
	c.OpenRouterKey = ""
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("a missing key was not named: %v", err)
	}
}

// The key rides on every brokered request, so the upstream must be https --
// except on loopback, where a test server stands in and there is no wire.
//
// A PATH is accepted, and has to be: OpenRouter's Messages endpoint lives under
// /api, and the proxy joins that with the SDK's own /v1/messages. A query is
// still refused -- the guest's ?beta=true would be concatenated onto it -- and
// so is a value that did not parse at all.
func TestUpstreamMustBeHTTPSUnlessLoopback(t *testing.T) {
	for _, ok := range []string{"https://openrouter.ai/api", "https://openrouter.ai/api/",
		"http://127.0.0.1:9", "http://localhost:9", "http://[::1]:9"} {
		c := validConfig()
		c.OpenRouterUpstream = mustURL(ok)
		if err := c.validate(); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	// api.anthropic.com is NOT in the accepted list, deliberately: the broker
	// authenticates with a bearer token now, which Anthropic reads as an OAuth
	// token, so that upstream would 401 every turn. Accepting it here would
	// advertise a rollback that does not work.
	for _, bad := range []string{"http://openrouter.ai/api",
		"https://openrouter.ai/api?x=1", "openrouter.ai/api", "",
		"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/",
		"https://openrouter.ai/api/v1/messages"} {
		c := validConfig()
		c.OpenRouterUpstream = mustURL(bad)
		if err := c.validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	c := validConfig()
	c.OpenRouterUpstream = nil
	if err := c.validate(); err == nil {
		t.Error("an unparsable upstream was accepted")
	}
}
