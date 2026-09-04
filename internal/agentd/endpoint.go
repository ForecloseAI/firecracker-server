package agentd

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// brokerPort is where the host lends its model credential, on this guest's own
// tap gateway. The same port as the connected-apps broker: one hole in the
// firewall, two things behind it.
const brokerPort = "8092"

// brokerKey is what a brokered request presents. The SDK refuses to send a
// request with no credential at all, and the broker replaces whatever it sees,
// so any non-empty string does; this one says what it is in a capture.
const brokerKey = "brokered"

// routeTable is the kernel's routing table. The guest's default route is its
// tap gateway, which is the host, which is where the broker listens. A var so
// a test can point it at a fixture.
var routeTable = "/proc/net/route"

// endpoint is where one agent's model calls go and what they present.
//
// Empty means the SDK's own environment: a credential in ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN, and whatever ANTHROPIC_BASE_URL says. That is how a
// developer's laptop and every offline test run. A guest has none of those --
// the image ships no credential on purpose -- and dials the host instead.
type endpoint struct {
	baseURL string
	key     string
	// err is why the broker could not be located, kept for the startup line.
	// Turns on such an endpoint fail, and the log should already say why.
	err error
}

// defaultEndpoint decides how this process reaches the model.
//
// A credential in the environment wins outright, ANTHROPIC_BASE_URL included,
// so nothing changes for anyone who has one. Without one, an explicit base URL
// names the broker; without that too, the broker is the default gateway.
func defaultEndpoint() endpoint {
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return endpoint{}
	}
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
		return endpoint{baseURL: base, key: brokerKey}
	}
	gw, err := gatewayIP()
	if err != nil {
		return endpoint{err: err}
	}
	return endpoint{baseURL: "http://" + net.JoinHostPort(gw, brokerPort), key: brokerKey}
}

// newClient builds the SDK client for an endpoint. Environment defaults are
// dropped for a brokered one, so a stray ANTHROPIC_BASE_URL or a credential
// file cannot redirect it -- which also drops the SDK's own HTTP client, hence
// brokerHTTPClient.
func newClient(ep endpoint) anthropic.Client {
	if ep.baseURL == "" {
		return anthropic.NewClient()
	}
	return anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithHTTPClient(brokerHTTPClient()),
		option.WithBaseURL(ep.baseURL),
		option.WithAPIKey(ep.key),
	)
}

// brokerHTTPClient is the SDK's default transport, rebuilt: a response-header
// timeout so a broker that accepts the connection and never answers fails the
// turn instead of hanging it. The body is not covered, so a long stream is
// unaffected.
func brokerHTTPClient() *http.Client {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Transport: http.DefaultTransport}
	}
	t = t.Clone()
	t.ResponseHeaderTimeout = 10 * time.Minute
	return &http.Client{Transport: t}
}

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
	case ep.baseURL == "":
		return "the credential in the environment"
	default:
		return "the broker at " + ep.baseURL
	}
}

// DescribeEndpoint is the startup line's half of the decision every agent makes
// when it is built, so the log and the agents cannot disagree.
func DescribeEndpoint() string { return defaultEndpoint().String() }
