package chat

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "__Host-sess"
	sessionTTL    = 30 * 24 * time.Hour
	lockoutAfter  = 5
	lockoutMax    = 15 * time.Minute
	limiterCap    = 10_000
)

// session is one logged-in browser.
type session struct {
	user    string
	expires time.Time
}

// attempt tracks failed logins for one username.
type attempt struct {
	failures int
	until    time.Time
}

// Auth owns sessions and login throttling.
type Auth struct {
	mu       sync.Mutex
	creds    *Creds
	sessions map[string]session
	attempts map[string]*attempt
}

// NewAuth builds an auth store over the given credentials.
func NewAuth(c *Creds) *Auth {
	return &Auth{creds: c, sessions: map[string]session{}, attempts: map[string]*attempt{}}
}

// SetCreds swaps the credential set, for SIGHUP reload.
func (a *Auth) SetCreds(c *Creds) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creds = c
}

// Login checks the password under the throttle and returns a new session token.
func (a *Auth) Login(user, password string) (string, bool) {
	if !a.allow(user) {
		return "", false
	}
	if !a.credentials().Verify(user, password) {
		a.fail(user)
		return "", false
	}
	a.succeed(user)
	return a.mint(user), true
}

// credentials reads the current set under the lock.
func (a *Auth) credentials() *Creds {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creds
}

// allow reports whether this username is currently allowed to try. Keyed on
// username, not IP: behind a reverse proxy every request shares a RemoteAddr,
// so IP keying would be a global lockout switch.
func (a *Auth) allow(user string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.attempts[user]
	return !ok || time.Now().After(at.until)
}

// fail records a failure and extends the backoff.
func (a *Auth) fail(user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evict()
	at, ok := a.attempts[user]
	if !ok {
		at = &attempt{}
		a.attempts[user] = at
	}
	at.failures++
	if at.failures > lockoutAfter {
		at.until = time.Now().Add(backoff(at.failures))
	}
}

// backoff doubles per failure past the threshold, capped.
func backoff(failures int) time.Duration {
	d := time.Second << min(failures-lockoutAfter, 10)
	return min(d, lockoutMax)
}

// succeed clears the throttle for a username.
func (a *Auth) succeed(user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.attempts, user)
}

// evict bounds the throttle map so it cannot be grown into an OOM.
func (a *Auth) evict() {
	if len(a.attempts) < limiterCap {
		return
	}
	for k := range a.attempts {
		delete(a.attempts, k)
		if len(a.attempts) < limiterCap/2 {
			return
		}
	}
}

// mint creates a session token.
func (a *Auth) mint(user string) string {
	buf := make([]byte, 32)
	rand.Read(buf)
	tok := base64.RawURLEncoding.EncodeToString(buf)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweep()
	a.sessions[tok] = session{user: user, expires: time.Now().Add(sessionTTL)}
	return tok
}

// sweep drops expired sessions.
func (a *Auth) sweep() {
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
	}
}

// User returns the logged-in username for a request, if any.
func (a *Auth) User(r *http.Request) (string, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[ck.Value]
	if !ok || time.Now().After(s.expires) {
		return "", false
	}
	return s.user, true
}

// Logout drops the session server-side.
func (a *Auth) Logout(r *http.Request) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, ck.Value)
}

// setSession writes the session cookie. The __Host- prefix requires Secure,
// Path=/ and an EMPTY Domain, which is what structurally prevents this cookie
// from ever reaching the VNC origin that serves untrusted guest HTML.
func setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL / time.Second),
	})
}

// clearSession expires the cookie in the browser.
func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
