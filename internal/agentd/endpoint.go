package agentd

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// brokerPort is where the host lends its model credential, on this guest's own
// tap gateway. The same port as the connected-apps broker -- CHAT_APPS_ADDR's
// default in internal/chat/config.go -- so one hole in the firewall serves both.
// Stated here rather than imported because the guest cannot depend on chat.
const brokerPort = "8092"

// brokerKey is what a brokered request presents. The SDK refuses to send a
// request with no credential at all, and the broker replaces whatever it sees,
// so any non-empty string does; this one says what it is in a capture.
const brokerKey = "brokered"

// appURL and appName are what OpenRouter attributes a request to, in its
// HTTP-Referer and X-Title headers: the service making the call and its name.
// appURL is CHAT_ORIGIN -- see internal/chat/config.go -- stated again rather
// than imported, because the guest cannot depend on chat. Constants, not
// config: they say who we are, which does not vary per machine.
const (
	appURL  = "https://chat.usetypeo.com"
	appName = "AutoBots"
)

// openRouterBase is where model calls go when this process has a key of its own
// -- a laptop, not a guest. No "/v1": the SDK appends that itself, and
// OpenRouter serves the Messages endpoint under /api. See modelBase.
const openRouterBase = "https://openrouter.ai/api"

// routeTable is the kernel's routing table. The guest's default route is its
// tap gateway, which is the host, which is where the broker listens. A var so
// a test can point it at a fixture.
var routeTable = "/proc/net/route"

// endpoint is where one agent's model calls go and what they present.
//
// A guest ships no credential on purpose and dials the host's broker. A laptop
// with OPENROUTER_API_KEY calls OpenRouter directly on it, which is the same
// request over the same protocol -- so what is proved locally is what runs.
type endpoint struct {
	baseURL string
	key     string
	// model and thinking are per agent: a profile's model, or for a custom
	// agent whatever the person chose, with how much it should reason.
	model    string
	thinking string
	// plain marks an endpoint whose capabilities we do not know, which is any
	// URL the person pasted. Requests to it carry no betas and no context
	// management, since a service that merely speaks the API need not honour
	// them. Our own broker is NOT plain: it goes to OpenRouter, which documents
	// context_management and cache_control as request fields.
	//
	// This used to be called foreign and mean "not Anthropic". That stopped
	// being the distinction the moment the broker's own upstream stopped being
	// Anthropic; what it has always actually gated is whether we can rely on
	// the extras, and a pasted URL is where we cannot.
	plain bool
	// bearer says to authenticate with Authorization rather than the x-api-key
	// header alone, and to say who is calling. True where the key is the
	// caller's own AND the endpoint is not Anthropic itself: OpenRouter wants a
	// bearer, Anthropic wants the key header, and a brokered request wants
	// neither because the host sets both on its own key.
	bearer bool
	// summary is the cheap model to compact with, in the id dialect this
	// endpoint speaks: OpenRouter prefixes by provider, Anthropic does not, and
	// each rejects the other's spelling. Empty means we know of no cheap model
	// here, and compaction runs on the agent's own.
	//
	// It has to travel with the endpoint rather than be one constant, because
	// compaction is the call nobody watches: a wrong id there does not fail a
	// turn, it fails inside compactIfNeeded, which logs and returns, so the
	// conversation silently stops being trimmed and re-pays its whole history
	// on every turn after that.
	summary string
	// err is why the broker could not be located, kept for the startup line.
	// Turns on such an endpoint fail, and the log should already say why.
	err error
}

// defaultEndpoint decides how this process reaches the model. Decided once,
// by the supervisor, so the startup line and every agent agree.
//
// A key of this process's own wins outright and calls OpenRouter directly:
// that is a laptop, and it is deliberately the same request the fleet makes.
// Without one, ANTHROPIC_BASE_URL names a broker explicitly -- which is how the
// tests point at an httptest server -- and without that, the broker is this
// guest's default gateway.
//
// There is no longer a branch that hands the SDK its own environment. It used
// to mean "a developer with an Anthropic key calls Anthropic directly", and
// both halves of that are gone: the key is retired, and the model ids the
// profiles now ask for are OpenRouter slugs Anthropic would reject.
func defaultEndpoint() endpoint {
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		return endpoint{baseURL: openRouterBase, key: key, bearer: true, summary: summaryOpenRouter}
	}
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
		return endpoint{baseURL: base, key: brokerKey, summary: summaryOpenRouter}
	}
	gw, err := gatewayIP()
	if err != nil {
		return endpoint{err: err}
	}
	return endpoint{baseURL: "http://" + net.JoinHostPort(gw, brokerPort), key: brokerKey,
		summary: summaryOpenRouter}
}

// forAgent is this endpoint with the model one agent should call -- or, when
// the person gave the agent a model of its own, that endpoint instead, on their
// key.
//
// One question decides both flags: is this Anthropic itself? If not, it is a
// gateway we know nothing about, so requests go plain and authenticate the way
// OpenRouter does. If it is, nothing changes from the day this path was written
// -- their key in the x-api-key header, betas and all.
func (ep endpoint) forAgent(model string, own *agentapi.ModelConfig) endpoint {
	if own == nil {
		ep.model = model
		return ep
	}
	base := modelBase(own.URL)
	u, err := url.Parse(base)
	notAnthropic := err != nil || u.Hostname() != "api.anthropic.com"
	ep2 := endpoint{baseURL: base, key: own.APIKey, model: own.Model, thinking: own.Thinking,
		plain: notAnthropic, bearer: notAnthropic}
	if !notAnthropic {
		// Anthropic knows the cheap model, under its own unprefixed name.
		ep2.summary = summaryAnthropic
	}
	return ep2
}

// modelBase trims a pasted base URL back to what the SDK expects. The SDK
// appends "v1/messages" itself, and OpenRouter documents its endpoint as
// .../api/v1, so a URL copied from those docs would otherwise dial v1 twice.
func modelBase(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	for _, suffix := range []string{"/v1/chat/completions", "/v1/messages", "/v1"} {
		if strings.HasSuffix(trimmed, suffix) {
			return strings.TrimSuffix(trimmed, suffix)
		}
	}
	return trimmed
}

// newClient builds the SDK client for an endpoint. One construction path for
// both kinds: the transport below is a property of a model call, not of where
// the key came from, so a hang reproduces the same on a laptop as in a guest.
// A brokered endpoint also drops the SDK's environment defaults, so a stray
// ANTHROPIC_BASE_URL or credential file cannot redirect it.
func newClient(ep endpoint) anthropic.Client {
	// WithoutEnvironmentDefaults goes on unconditionally, including on the
	// endpoint that failed to find a broker. Applying it only alongside a base
	// URL left exactly that case reading the SDK's environment, so a stray
	// ANTHROPIC_API_KEY in a guest would quietly route turns to Anthropic --
	// where the profiles' prefixed ids are rejected -- instead of failing the
	// loud way the startup line promises.
	opts := []option.RequestOption{option.WithHTTPClient(modelHTTP()),
		option.WithoutEnvironmentDefaults()}
	if ep.baseURL != "" {
		opts = append(opts, option.WithBaseURL(ep.baseURL), option.WithAPIKey(ep.key))
	}
	// An endpoint carrying a key of our own that is not Anthropic also gets a
	// bearer credential, which is how OpenRouter authenticates, and the two
	// headers it attributes traffic by. Both auth shapes go rather than one: it
	// is the same key to the same host either way, so the second copy tells
	// nobody new, and a proxy that wanted the key header keeps working on it.
	if ep.bearer {
		opts = append(opts, option.WithAuthToken(ep.key),
			option.WithHeader("HTTP-Referer", appURL), option.WithHeader("X-Title", appName))
	}
	return anthropic.NewClient(opts...)
}

// modelHTTP is the one HTTP client every agent's model calls share, so
// connections are pooled across agents rather than opened afresh by each. It is
// the SDK's own default rebuilt: a response-header timeout, so a broker that
// accepts the connection and never answers fails the turn instead of hanging
// it. The body is not covered, so a long stream is unaffected.
var modelHTTP = sync.OnceValue(func() *http.Client {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Transport: http.DefaultTransport}
	}
	t = t.Clone()
	t.ResponseHeaderTimeout = 10 * time.Minute
	return &http.Client{Transport: t}
})

// gatewayIP reads the default gateway out of the kernel's routing table.
func gatewayIP() (string, error) {
	buf, err := os.ReadFile(routeTable)
	if err != nil {
		return "", err
	}
	gw, ok := parseRoute(string(buf))
	if !ok {
		return "", errors.New("no default route in " + routeTable)
	}
	return gw, nil
}

// parseRoute finds the default gateway in /proc/net/route text.
//
// A header line, then one line per route with Iface, Destination, Gateway and
// Flags in hex. The default route has destination 00000000 and the RTF_GATEWAY
// flag (0x2); a default-looking route without that flag is a device route and
// names no gateway. Addresses are little-endian: 172.16.0.1 reads 010010AC.
func parseRoute(table string) (string, bool) {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x2 == 0 {
			continue
		}
		if ip, ok := hexIPv4(f[2]); ok {
			return ip, true
		}
	}
	return "", false
}

// hexIPv4 decodes a little-endian hex address from the routing table.
func hexIPv4(h string) (string, bool) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 {
		return "", false
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0]), true
}

// String says, for the startup log, how model calls will travel.
func (ep endpoint) String() string {
	switch {
	case ep.err != nil:
		return "no broker (" + ep.err.Error() + "); every turn will fail"
	case ep.bearer:
		return ep.baseURL + " on this process's own key"
	default:
		return "the broker at " + ep.baseURL
	}
}
