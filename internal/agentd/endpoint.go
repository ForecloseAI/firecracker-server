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

// appURL is what OpenRouter attributes a request to, in its HTTP-Referer
// header. It is CHAT_ORIGIN -- see internal/chat/config.go -- stated again
// rather than imported, because the guest has no CHAT_ORIGIN to read. The name
// beside it does not need restating: agentapi.AppName serves both sides.
const appURL = "https://chat.usetypeo.com"

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
	// bearer says to authenticate with Authorization rather than the x-api-key
	// header alone, and to say who is calling. True where the key is the
	// caller's own AND the endpoint is not Anthropic itself: OpenRouter wants a
	// bearer, Anthropic wants the key header, and a brokered request wants
	// neither because the host sets both on its own key.
	bearer bool
	// summary is the cheap model to compact with, in the id dialect this
	// endpoint speaks -- OpenRouter prefixes by provider, Anthropic does not,
	// and each rejects the other's spelling. Empty is the whole of what we know
	// about an endpoint somebody pasted a URL for: see known.
	//
	// It travels with the endpoint rather than being one constant because
	// compaction is the call nobody watches. A wrong id there does not fail a
	// turn; it fails inside compactIfNeeded, which logs and returns, so the
	// conversation silently stops being trimmed and re-pays its whole history
	// on every turn after that.
	summary string
	// err is why the broker could not be located, kept for the startup line.
	// Turns on such an endpoint fail, and the log should already say why.
	err error
}

// known says whether this is a service we recognise, which is what decides
// whether the extras can be relied on: betas and context management go only to
// an endpoint we chose, never to a URL somebody pasted, because a service that
// merely speaks the API need not honour them. Knowing its cheap model is the
// same knowledge, so one field answers both.
func (ep endpoint) known() bool { return ep.summary != "" }

// defaultEndpoint decides how this process reaches the model. Decided once,
// by the supervisor, so the startup line and every agent agree.
//
// A key of this process's own wins outright and calls OpenRouter directly:
// that is a laptop, and it is deliberately the same request the fleet makes.
// Without one, ANTHROPIC_BASE_URL names a broker explicitly -- which is how the
// tests point at an httptest server -- and without that, the broker is this
// guest's default gateway.
//
// No branch hands the SDK its own environment: an Anthropic key reaches nothing
// here, since the ids the profiles ask for are OpenRouter slugs it would reject.
func defaultEndpoint() endpoint {
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		return endpoint{baseURL: agentapi.OpenRouterBase, key: key, bearer: true, summary: summaryOpenRouter}
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
	base, _ := agentapi.TrimSDKSuffix(own.URL)
	u, err := url.Parse(base)
	isAnthropic := err == nil && u.Hostname() == "api.anthropic.com"
	ep = endpoint{baseURL: base, key: own.APIKey, model: own.Model,
		thinking: own.Thinking, bearer: !isAnthropic}
	if isAnthropic {
		ep.summary = summaryAnthropic // Anthropic knows the cheap model, unprefixed.
	}
	return ep
}

// newClient builds the SDK client for an endpoint. One construction path for
// all of them: the transport below is a property of a model call, not of where
// the key came from, so a hang reproduces the same on a laptop as in a guest.
//
// The SDK's environment defaults are dropped for every endpoint, including the
// one that failed to find a broker -- that endpoint has no base URL, so a
// version of this that dropped them only alongside one left exactly the broken
// case reading a stray ANTHROPIC_API_KEY and quietly reaching Anthropic instead
// of failing the loud way the startup line promises.
func newClient(ep endpoint) anthropic.Client {
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
			option.WithHeader("HTTP-Referer", appURL), option.WithHeader("X-Title", agentapi.AppName))
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
