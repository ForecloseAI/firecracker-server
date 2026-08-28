package chat

import (
	"fmt"
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
	}
	return c, c.validate()
}

// validate fails fast rather than half-working. The https check matters: the
// session cookie uses the __Host- prefix, which browsers reject without it.
func (c Config) validate() error {
	for _, f := range []struct{ name, val string }{
		{"CHAT_ORIGIN", c.Origin}, {"CHAT_VNC_ORIGIN", c.VNCOrigin}, {"CRACKED_TOKEN", c.Token},
		{"SUPABASE_URL", c.SupabaseURL},
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
	return nil
}

// env reads a variable with a fallback.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
