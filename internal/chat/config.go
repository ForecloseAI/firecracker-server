package chat

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// Config is the whole runtime configuration, read once from the environment.
type Config struct {
	Addr       string
	VNCAddr    string
	Origin     string
	VNCOrigin  string
	ControlURL string
	Token      string
	// SupabaseURL is the project the app authenticates against. Its public keys
	// are what every /v1 request is verified with, and its hostname is the
	// issuer those tokens must claim.
	SupabaseURL string
	// LogBodies puts request and response bodies in the log. On by default
	// because the app is being integrated against; set CHAT_LOG_BODIES=0 once
	// real people are using it, since message text is their content.
	LogBodies bool

	// SupabasePublishable identifies the project to PostgREST. Not a secret: it
	// is the same key the app ships with, and it grants nothing on its own --
	// row-level security plus the caller's own token decide what a request sees.
	SupabasePublishable string

	// ComposioKey is authority over every user's connected accounts, so it lives
	// only here and never reaches a guest. Empty turns connected apps off
	// entirely, which is why the feature needs no flag of its own.
	ComposioKey string
	// ComposioBase overrides the provider's REST root, for a test host.
	ComposioBase string
	// AppsAddr is the guest listener: the connected-apps broker and the model
	// broker share it. Bound on every interface rather than loopback, because
	// it has to be reachable from a tap device -- the firewall and each
	// broker's own gate are what keep it shut, and the port is not in the
	// security group.
	AppsAddr string

	// OpenRouterKey is the model credential every guest's turns run on. It lives
	// ONLY here: the guest image carries none, and agentd dials this host's
	// broker for every model call. Required, because a chat host without it is
	// every machine on the fleet unable to take a turn.
	OpenRouterKey string
	// OpenRouterUpstream is where brokered model calls go. An origin, optionally
	// with a path -- OpenRouter serves its Anthropic-Messages endpoint under
	// /api -- and the guest's own path and query ride through on top of it.
	// Parsed here, once, so the broker is handed something already known sound.
	OpenRouterUpstream *url.URL
	// ComposioCallback is where a person lands after approving an app. It has to
	// be a page this service serves, because the browser coming back from a
	// provider carries no token and a custom scheme is not reliably followed
	// through a server redirect.
	ComposioCallback string
}

// LoadConfig reads and validates the environment.
func LoadConfig() (Config, error) {
	c := Config{
		Addr:        env("CHAT_ADDR", "127.0.0.1:8090"),
		VNCAddr:     env("CHAT_VNC_ADDR", "127.0.0.1:8091"),
		Origin:      env("CHAT_ORIGIN", ""),
		VNCOrigin:   env("CHAT_VNC_ORIGIN", ""),
		ControlURL:  env("CRACKED_URL", "http://127.0.0.1:8080"),
		Token:       env("CRACKED_TOKEN", ""),
		SupabaseURL: strings.TrimSuffix(env("SUPABASE_URL", ""), "/"),
		LogBodies:   env("CHAT_LOG_BODIES", "1") != "0",

		SupabasePublishable: env("SUPABASE_PUBLISHABLE_KEY", ""),
		ComposioKey:         env("COMPOSIO_API_KEY", ""),
		ComposioBase:        env("COMPOSIO_BASE_URL", ""),
		AppsAddr:            env("CHAT_APPS_ADDR", "0.0.0.0:8092"),

		OpenRouterKey: env("OPENROUTER_API_KEY", ""),
	}
	// A parse failure leaves nil, which validate refuses by name.
	c.OpenRouterUpstream, _ = url.Parse(env("OPENROUTER_UPSTREAM", "https://openrouter.ai/api"))
	c.ComposioCallback = env("COMPOSIO_CALLBACK_URL", c.Origin+connectedPath)
	return c, c.validate()
}

// validate fails fast rather than half-working. The https check matters: the
// session cookie uses the __Host- prefix, which browsers reject without it.
func (c Config) validate() error {
	for _, f := range []struct{ name, val string }{
		{"CHAT_ORIGIN", c.Origin}, {"CHAT_VNC_ORIGIN", c.VNCOrigin}, {"CRACKED_TOKEN", c.Token},
		{"SUPABASE_URL", c.SupabaseURL}, {"OPENROUTER_API_KEY", c.OpenRouterKey},
	} {
		if f.val == "" {
			return fmt.Errorf("%s must be set", f.name)
		}
	}
	if !strings.HasPrefix(c.Origin, "https://") {
		return fmt.Errorf("CHAT_ORIGIN must be https, or the session cookie will be rejected")
	}
	if !strings.HasPrefix(c.SupabaseURL, "https://") {
		return fmt.Errorf("SUPABASE_URL must be https, or access tokens travel in the clear")
	}
	// Required only when connected apps are on. Making it unconditional would
	// stop every existing deployment coming up on the next restart, for a key
	// nothing reads unless a provider is configured.
	if c.ComposioKey != "" && c.SupabasePublishable == "" {
		return fmt.Errorf("SUPABASE_PUBLISHABLE_KEY must be set when COMPOSIO_API_KEY is")
	}
	return c.validateGuest()
}

// validateGuest checks the guest listener and where its model broker forwards.
// The listener always opens, so its address is checked unconditionally; a
// non-numeric port is left to listen(), which fails loudly on its own.
func (c Config) validateGuest() error {
	if _, _, err := net.SplitHostPort(c.AppsAddr); err != nil {
		return fmt.Errorf("CHAT_APPS_ADDR must be host:port: %w", err)
	}
	if err := validUpstream(c.OpenRouterUpstream); err != nil {
		return fmt.Errorf("OPENROUTER_UPSTREAM %w", err)
	}
	return nil
}

// validUpstream accepts an https origin, optionally with a path, or plain http
// on loopback for a test server. The key rides on every request, so anything
// else in the clear is refused.
//
// A path is allowed because OpenRouter's Messages endpoint lives under /api and
// the proxy joins the two -- /api plus the SDK's /v1/messages is the real URL. A
// query is not: the guest's own would be concatenated onto it, and the SDK sends
// ?beta=true on every call, so there is no shape where a second one helps.
func validUpstream(u *url.URL) error {
	if u == nil || u.Host == "" {
		return errors.New("must be an absolute URL")
	}
	if u.RawQuery != "" {
		return errors.New("must not carry a query")
	}
	// The SDK appends v1/messages itself, and every OpenRouter doc page shows
	// the base as .../api/v1 -- so the obvious copy-paste dials /api/v1/v1/
	// messages and 404s the whole fleet on a value that looks right. The guest
	// trims this for a pasted URL (modelBase); here it is refused by name,
	// because an operator can fix a startup error and cannot see a 404 a guest
	// gets an hour later.
	if p := strings.TrimRight(u.Path, "/"); strings.HasSuffix(p, "/v1") ||
		strings.HasSuffix(p, "/v1/messages") {
		return errors.New("must not end in /v1 or /v1/messages; the SDK appends those")
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil
	}
	return errors.New("must be https, or the key travels in the clear")
}

// isLoopback says whether a host names this machine, where plaintext is fine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// env reads a variable with a fallback.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
