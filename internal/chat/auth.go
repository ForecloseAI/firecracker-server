package chat

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"cracked/internal/vm"
)

// Identity is who a request is from, as proven by a Supabase access token.
//
// UserID is the token's `sub`: a UUID minted by Supabase and stable for the life
// of the account. It is the only thing this service keys anything on. Email is
// carried for the log line and nothing else -- people change their email address
// and the id survives that, so nothing may branch on it.
type Identity struct {
	UserID string
	Email  string
}

// audience is the audience Supabase stamps on a signed-in user's access token.
// Anonymous and service tokens carry other values and must not be accepted here.
const audience = "authenticated"

// Verifier checks Supabase access tokens against the project's public keys.
//
// The keys are asymmetric (ES256/RS256) and fetched from the project's JWKS
// endpoint, so this service holds no secret capable of minting a token: the
// worst a leak of anything here can do is verify. keyfunc keeps the key set
// fresh in the background, which is what makes a key rotation in the Supabase
// dashboard a thing that needs no redeploy.
type Verifier struct {
	keys   keyfunc.Keyfunc
	issuer string
}

// NewVerifier fetches the project's public keys and prepares to verify.
//
// The fetch happens here, at startup, rather than on the first request: a
// misconfigured SUPABASE_URL should stop the service coming up, not surface as
// every user being told they are unauthorized.
func NewVerifier(ctx context.Context, supabaseURL string) (*Verifier, error) {
	base := strings.TrimSuffix(supabaseURL, "/")
	keys, err := keyfunc.NewDefaultCtx(ctx, []string{base + "/auth/v1/.well-known/jwks.json"})
	if err != nil {
		return nil, fmt.Errorf("supabase jwks: %w", err)
	}
	return &Verifier{keys: keys, issuer: base + "/auth/v1"}, nil
}

// identify verifies the token on a request and returns who it belongs to.
//
// Everything here is checked locally against a cached public key, so this costs
// no network round trip and is safe to run on every request. It is signature,
// expiry, issuer and audience only: a token revoked at Supabase stays valid here
// until it expires, which is the standard trade and the reason access tokens are
// short-lived.
func (v *Verifier) identify(r *http.Request) (Identity, bool) {
	raw := requestToken(r)
	if raw == "" {
		return Identity{}, false
	}
	var claims struct {
		Email string `json:"email"`
		jwt.RegisteredClaims
	}
	_, err := jwt.ParseWithClaims(raw, &claims, v.keys.Keyfunc,
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil || claims.Subject == "" {
		return Identity{}, false
	}
	return Identity{UserID: claims.Subject, Email: claims.Email}, true
}

// uuidRe is the exact shape of a Supabase user id: 8-4-4-4-12 hex.
//
// Matching strictly, rather than accepting whatever normalises into a usable id,
// is what keeps the mapping injective. "abc" and "a-bc" both strip to "abc", so
// a lenient rule would hand two different accounts the same machine -- and a
// shared machine is a shared roster, shared threads and shared files.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// machineFor is the VM belonging to a Supabase user.
//
// The id is derived, not stored: a UUID with its hyphens removed is exactly 32
// lowercase hex characters, which is exactly what the control plane's id shape
// allows (^[a-z0-9][a-z0-9-]{0,31}$). That equality is load-bearing -- it is why
// this service needs no user table to remember whose machine is whose, and why a
// user who has never been seen before still resolves to a machine that
// ensureMachine can boot on demand.
//
// Anything that is not a UUID gets no machine, and guestOf turns that into a
// clean error rather than an attempt to boot something surprising.
func machineFor(userID string) string {
	lower := strings.ToLower(userID)
	if !uuidRe.MatchString(lower) {
		return ""
	}
	id := strings.ReplaceAll(lower, "-", "")
	if !vm.ValidID(id) {
		return ""
	}
	return id
}

// requestToken pulls the access token from the header or the query string.
//
// Two doors, not three: the app sends a header, and the event stream sends a
// query parameter because neither a browser nor React Native can set headers on
// an SSE connection. The __Host-sess cookie is deliberately not read here -- it
// carries the operator token for the built-in page, which is a different
// credential for a different surface, and accepting it here would let an
// operator's session act as any user.
func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return h[7:]
	}
	return r.URL.Query().Get("token")
}

// identityKey is the context key the logging middleware stores an Identity under.
type identityKey struct{}

// withIdentity returns a request carrying a verified identity.
func withIdentity(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
}

// identityFrom returns the identity the logging middleware verified, if there
// was one. Verification happens once, at the outermost wrapper; every guard
// below reads the answer rather than parsing the token again.
func identityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// tokenKey is the context key the verified access token is carried under.
type tokenKey struct{}

// withToken returns a request carrying the caller's own access token.
//
// A SECOND context value rather than a field on Identity, deliberately. Identity
// is documented as carried for the log line, and a bearer token has no business
// in a struct that exists to be logged. This one is read by exactly one caller:
// the store, which forwards it so the database -- not a WHERE clause somebody
// remembered to write -- decides which rows the request may see.
func withToken(r *http.Request, raw string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), tokenKey{}, raw))
}

// tokenFrom returns the caller's access token, if this request had one.
func tokenFrom(ctx context.Context) string {
	raw, _ := ctx.Value(tokenKey{}).(string)
	return raw
}
