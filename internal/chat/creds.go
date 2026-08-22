// Package chat serves the browser-facing chat UI. It holds the control-plane
// token and reaches guests server-side, so the browser never opens a socket to
// a VM and no fleet credential is ever exposed to a page.
package chat

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// PBKDF2-HMAC-SHA256 at the current OWASP figure. Stored per record so the
// count can be raised later without a flag day.
const (
	iterations = 600_000
	keyLen     = 32
	saltLen    = 16
)

// record is one user's stored password verifier.
type record struct {
	salt []byte
	key  []byte
	iter int
}

// Creds maps username to verifier, plus a dummy used to keep the timing of an
// unknown user indistinguishable from a wrong password.
type Creds struct {
	users map[string]record
	dummy record
}

// LoadCreds reads the users file: one "name:pbkdf2-sha256$iter$salt$key" line.
func LoadCreds(path string) (*Creds, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	users, err := parseRecords(string(raw))
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%s has no users", path)
	}
	return &Creds{users: users, dummy: newRecord("unused-placeholder")}, nil
}

// parseRecords turns the file body into verifiers, skipping blanks and comments.
func parseRecords(body string) (map[string]record, error) {
	out := map[string]record{}
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: want name:hash", i+1)
		}
		rec, err := parseRecord(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out[name] = rec
	}
	return out, nil
}

// parseRecord decodes one "pbkdf2-sha256$iter$salt$key" verifier.
func parseRecord(s string) (record, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return record{}, fmt.Errorf("unknown hash format")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 {
		return record{}, fmt.Errorf("bad iteration count")
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return record{}, fmt.Errorf("bad base64")
	}
	return record{salt: salt, key: key, iter: iter}, nil
}

// derive runs the KDF. The stdlib version returns an error, unlike x/crypto.
func derive(password string, salt []byte, iter int) []byte {
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, keyLen)
	if err != nil {
		return make([]byte, keyLen)
	}
	return key
}

// Verify reports whether the password is right. An unknown user still pays the
// full derivation cost against a dummy: at 600k iterations, returning early
// would make the difference trivially observable.
func (c *Creds) Verify(user, password string) bool {
	rec, known := c.users[user]
	if !known {
		rec = c.dummy
	}
	got := derive(password, rec.salt, rec.iter)
	ok := subtle.ConstantTimeCompare(got, rec.key) == 1
	return ok && known
}

// newRecord hashes a password with a fresh salt.
func newRecord(password string) record {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	return record{salt: salt, key: derive(password, salt, iterations), iter: iterations}
}

// HashPassword renders a "name:verifier" line for the users file.
func HashPassword(user, password string) string {
	r := newRecord(password)
	return fmt.Sprintf("%s:pbkdf2-sha256$%d$%s$%s", user, r.iter,
		base64.RawStdEncoding.EncodeToString(r.salt),
		base64.RawStdEncoding.EncodeToString(r.key))
}
