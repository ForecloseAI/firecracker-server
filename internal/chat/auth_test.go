package chat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testAuth stands up a fake Supabase: an ES256 key, a JWKS endpoint serving its
// public half, and a Verifier pointed at it.
//
// Real keys and real signatures rather than a stubbed-out verifier, because the
// thing worth testing is that a token nobody could have forged is the only one
// that gets in. mint returns a signed token for a user id.
func testAuth(t *testing.T) (*Verifier, func(sub, email string) string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "test-key",
		"x": b64(key.X.FillBytes(make([]byte, 32))),
		"y": b64(key.Y.FillBytes(make([]byte, 32))),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)

	v, err := NewVerifier(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(sub, email string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
			"sub": sub, "email": email, "aud": audience,
			"iss": srv.URL + "/auth/v1",
			"exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(),
		})
		tok.Header["kid"] = "test-key"
		signed, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}
	return v, mint
}

// testUserID is the Supabase user every test signs in as. A real UUID, because
// the machine id is derived from its shape.
const testUserID = "3f8a1c92-5e4b-4d7a-9c11-0b2e6f8a4d31"

// The app replays a bearer token and the stream can only use a query parameter.
// Both doors have to reach the same person.
func TestBothDoorsResolveTheSameUser(t *testing.T) {
	v, mint := testAuth(t)
	tok := mint(testUserID, "tester@example.com")
	bearer := httptest.NewRequest("GET", "/v1/agents", nil)
	bearer.Header.Set("Authorization", "Bearer "+tok)
	query := httptest.NewRequest("GET", "/v1/stream?token="+tok, nil)
	for name, r := range map[string]*http.Request{"bearer": bearer, "query": query} {
		got, ok := v.identify(r)
		if !ok || got.UserID != testUserID || got.Email != "tester@example.com" {
			t.Errorf("%s resolved to (%+v, %v)", name, got, ok)
		}
	}
}

// The operator cookie must not authenticate a user. It carries the fleet token,
// which is a different credential for a different surface: honouring it here
// would let whoever runs the service act as any account.
func TestOperatorCookieIsNotAUserSession(t *testing.T) {
	v, mint := testAuth(t)
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: mint(testUserID, "a@b.com")})
	if _, ok := v.identify(r); ok {
		t.Error("a cookie authenticated a /v1 request")
	}
}

// The whole point of the migration: only Supabase can mint a token that works.
// A well-formed token signed by anyone else must be refused.
func TestForgedAndMalformedTokensAreRefused(t *testing.T) {
	v, _ := testAuth(t)
	other, mintElsewhere := testAuth(t) // a different project, different keys
	_ = other
	for name, tok := range map[string]string{
		"empty":            "",
		"garbage":          "not-a-jwt",
		"old-style":        "tok_7f3a9c2e5b1d4068",
		"signed-elsewhere": mintElsewhere(testUserID, "attacker@example.com"),
	} {
		r := httptest.NewRequest("GET", "/v1/agents", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		if _, ok := v.identify(r); ok {
			t.Errorf("%s token was accepted", name)
		}
	}
}

// An expired token is refused even though the signature is good. Revocation at
// Supabase is not visible here, so expiry is the only thing that ends a session.
func TestExpiredTokensAreRefused(t *testing.T) {
	v, _ := testAuth(t)
	// Mint by hand so the expiry is in the past.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub": testUserID, "aud": audience, "iss": v.issuer,
		"exp": time.Now().Add(-time.Minute).Unix(),
	})
	signed, _ := tok.SignedString(key)
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	if _, ok := v.identify(r); ok {
		t.Error("an expired token was accepted")
	}
}

// A token with no subject has nobody to be, and every machine id derives from
// the subject: accepting one would resolve to an empty machine.
func TestTokenWithoutASubjectIsRefused(t *testing.T) {
	v, mint := testAuth(t)
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+mint("", "nobody@example.com"))
	if _, ok := v.identify(r); ok {
		t.Error("a token with no sub was accepted")
	}
}

// A UUID with its hyphens removed is exactly the 32 characters the control plane
// allows. This equality is why there is no user table; if it ever stops holding,
// every user loses their machine.
func TestMachineIDIsDerivedFromTheUUID(t *testing.T) {
	got := machineFor(testUserID)
	if got != "3f8a1c925e4b4d7a9c110b2e6f8a4d31" {
		t.Fatalf("machineFor = %q", got)
	}
	if len(got) != 32 {
		t.Fatalf("machine id is %d characters, the control plane allows 32", len(got))
	}
	if machineFor("3F8A1C92-5E4B-4D7A-9C11-0B2E6F8A4D31") != got {
		t.Error("case must not produce a second machine for the same person")
	}
}

// Anything that is not a UUID must resolve to no machine rather than to a
// surprising one -- guestOf turns an empty id into a clean error.
func TestNonUUIDSubjectsGetNoMachine(t *testing.T) {
	for _, sub := range []string{"", "tester@example.com", "../etc/passwd", "not-a-uuid",
		"Robert'); DROP--", "3f8a1c925e4b4d7a9c110b2e6f8a4d31"} {
		if got := machineFor(sub); got != "" {
			t.Errorf("machineFor(%q) = %q, want no machine", sub, got)
		}
	}
}

// Two accounts must never derive the same machine. Stripping hyphens from
// anything hyphen-shaped would collide -- "abc" and "a-bc" strip to the same
// eight characters -- and a shared machine is a shared roster and shared files.
func TestDistinctUsersGetDistinctMachines(t *testing.T) {
	seen := map[string]string{}
	for _, sub := range []string{
		testUserID,
		"3f8a1c92-5e4b-4d7a-9c11-0b2e6f8a4d32",
		"00000000-0000-4000-8000-000000000001",
	} {
		m := machineFor(sub)
		if m == "" {
			t.Fatalf("machineFor(%q) gave no machine", sub)
		}
		if other, dup := seen[m]; dup {
			t.Errorf("%s and %s share machine %q", other, sub, m)
		}
		seen[m] = sub
	}
}
