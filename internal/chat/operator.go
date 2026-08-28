package chat

import (
	"crypto/subtle"
	"net/http"
)

// The built-in web page is an operator tool, not a user-facing surface.
//
// It used to sign in as one of the hardcoded testers, which gave it a user
// identity it no longer has: real users are Supabase accounts that exist only in
// the app. Rather than teach this page a second, cookie-shaped copy of the app's
// auth, it is gated the way the control plane's dashboard already is -- on the
// fleet token, held by whoever runs the service.
//
// That also closes a hole. The /api/* handlers take the VM id from the request
// body and never check it against the caller, so under the old scheme any signed
// -in tester could drive any other tester's machine. Behind the fleet token that
// is no longer a privilege escalation: it is the operator looking at their own
// fleet, which is what the page is for.

// sessionCookie carries the operator token for the built-in web page. The
// __Host- prefix requires Secure, Path=/ and an EMPTY Domain, which is what
// structurally prevents it from ever reaching the VNC origin that serves
// untrusted guest HTML. Never scope this to .usetypeo.com.
const sessionCookie = "__Host-sess"

// operatorToken pulls the fleet token from the header, the query string, or the
// cookie. The query door is how an operator arrives from a link; the cookie is
// what keeps the page working for the rest of the session.
func operatorToken(r *http.Request) string {
	if t := requestToken(r); t != "" {
		return t
	}
	if ck, err := r.Cookie(sessionCookie); err == nil {
		return ck.Value
	}
	return ""
}

// isOperator reports whether a request carries the fleet token. Compared in
// constant time, matching the control plane's guard.
func (s *Server) isOperator(r *http.Request) bool {
	tok := operatorToken(r)
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) == 1
}

// setSession stores the operator token so the page keeps working after the
// ?token= link that carried it is gone from the address bar.
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
