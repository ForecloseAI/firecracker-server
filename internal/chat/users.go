package chat

import "net/http"

// user is one login. Plain text, compared with ==, and checked into git -- this
// whole file is scaffolding for a handful of testers and is deleted the day a
// real identity provider lands. Do not grow it: if it ever needs hashing,
// expiry or revocation, that is the signal to stop editing it and adopt one.
type user struct {
	Email    string
	Password string
	Token    string
	// Machine is this person's own VM. It does not exist until they first sign
	// in -- the id is reserved here, and the machine is booted on demand. Agent
	// ids are NOT hardcoded alongside it: those come from the daemon at runtime.
	// Must match the control plane's id shape, ^[a-z0-9][a-z0-9-]{0,31}$.
	Machine string
}

// users is the entire user database. Add a tester by adding a line.
//
// The token is a fixed random string rather than the email, so knowing who uses
// the app is not the same as being able to drive their machine -- these calls
// create VMs and spend money on a model API.
var users = []user{
	{"naman@laikatest.com", "defaultpass", "tok_7f3a9c2e5b1d4068", "naman"},
	{"barkha@dell.com", "defaultpass", "tok_7f3a9c2e5b1d4058", "barkha"},
	{"verma@pgpsm.com", "defaultpass", "tok_7f3a9c2e3b1d4058", "verma"},
	{"nandini@easilygeo.com", "defaultpass", "tok_7f3a9c2e4b1d4058", "nandini"},
}

// machineFor is the VM belonging to a signed-in email.
func machineFor(email string) string {
	for _, u := range users {
		if u.Email == email {
			return u.Machine
		}
	}
	return ""
}

// login returns the token for an email and password, if they match.
func login(email, password string) (string, bool) {
	for _, u := range users {
		if u.Email == email && u.Password == password {
			return u.Token, true
		}
	}
	return "", false
}

// userFor resolves a request to a signed-in email.
func userFor(r *http.Request) (string, bool) {
	tok := requestToken(r)
	for _, u := range users {
		if tok != "" && u.Token == tok {
			return u.Email, true
		}
	}
	return "", false
}

// requestToken pulls the token from the header, the query string, or the cookie.
// Three doors because a native app has no cookie jar, the event stream cannot
// set headers at all, and the built-in web page only has a cookie.
func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return q
	}
	if ck, err := r.Cookie(sessionCookie); err == nil {
		return ck.Value
	}
	return ""
}

// sessionCookie carries the same token for the built-in web page. The __Host-
// prefix requires Secure, Path=/ and an EMPTY Domain, which is what structurally
// prevents it from ever reaching the VNC origin that serves untrusted guest HTML.
const sessionCookie = "__Host-sess"

// setSession writes the token as a cookie.
func setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400 * 30,
	})
}

// clearSession expires the cookie in the browser.
func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
