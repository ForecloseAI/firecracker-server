package chat

import (
	"strings"
	"testing"
)

// validConfig is the smallest environment the service accepts.
func validConfig() Config {
	return Config{
		Origin: "https://chat.example.com", VNCOrigin: "https://vnc.example.com",
		Token: "t", SupabaseURL: "https://p.supabase.co", AppsAddr: "0.0.0.0:8092",
		AnthropicKey: "sk-ant-x", AnthropicUpstream: "https://api.anthropic.com",
	}
}

// The guest image no longer carries a model credential, so a chat host without
// one is every machine on the fleet unable to take a turn. That has to fail at
// startup, by name, rather than one agent at a time.
func TestAnthropicKeyIsRequired(t *testing.T) {
	c := validConfig()
	if err := c.validate(); err != nil {
		t.Fatalf("a complete config was refused: %v", err)
	}
	c.AnthropicKey = ""
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("a missing key was not named: %v", err)
	}
}

// The key rides on every brokered request, so the upstream must be https --
// except on loopback, where a test server stands in and there is no wire. A
// path or query would be glued onto the SDK's own, so those are refused too.
func TestUpstreamMustBeHTTPSUnlessLoopback(t *testing.T) {
	for _, ok := range []string{"https://api.anthropic.com", "https://api.anthropic.com/",
		"http://127.0.0.1:9", "http://localhost:9", "http://[::1]:9"} {
		c := validConfig()
		c.AnthropicUpstream = ok
		if err := c.validate(); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://api.anthropic.com", "https://api.anthropic.com/v1",
		"https://api.anthropic.com?x=1", "api.anthropic.com", ""} {
		c := validConfig()
		c.AnthropicUpstream = bad
		if err := c.validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
