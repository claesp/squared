package web

import (
	"crypto/subtle"
	"errors"
	"net/http"

	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/sso"
	"squared/internal/store"
)

// The cookie that binds a sign-in attempt to the browser. Only the
// callback path receives it.
const ssoCookie = "squared_sso"

// ssoLogin starts a sign-in at the identity provider.
func (s *Server) ssoLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	l := sso.NewLogin()
	authURL, err := s.cfg.SSO.AuthURL(r.Context(), l)
	if err != nil {
		s.log.Error("start SSO sign-in", "error", err)
		s.loginError(w, r, http.StatusServiceUnavailable, next, "The identity provider is not available. Try again later.")
		return
	}
	token, err := s.svc.StartOIDCLogin(r.Context(), store.OIDCLogin{State: l.State, Nonce: l.Nonce, Verifier: l.Verifier, Next: next})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// SameSite=Lax: the browser sends the cookie when the provider sends
	// it back to the callback with a top-level GET request.
	http.SetCookie(w, &http.Cookie{
		Name: ssoCookie, Value: token, Path: "/auth/callback", MaxAge: 600,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// ssoCallback completes a sign-in. The provider sends the browser here
// with an authorization code and the state.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Path: "/auth/callback", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.log.Info("SSO sign-in refused by the provider", "error", e, "description", q.Get("error_description"))
		s.loginError(w, r, http.StatusUnauthorized, "/", "The identity provider did not sign you in.")
		return
	}
	c, err := r.Cookie(ssoCookie)
	if err != nil {
		s.loginError(w, r, http.StatusBadRequest, "/", "The sign-in took too long or started in a different browser. Try again.")
		return
	}
	l, err := s.svc.FinishOIDCLogin(r.Context(), c.Value)
	if errors.Is(err, domain.ErrNotFound) {
		s.loginError(w, r, http.StatusBadRequest, "/", "The sign-in took too long or was already used. Try again.")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The state protects against a forged callback (login CSRF).
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.State)) != 1 {
		s.log.Warn("SSO callback with wrong state", "ip", s.clientIP(r))
		s.loginError(w, r, http.StatusBadRequest, "/", "The sign-in is not valid. Try again.")
		return
	}
	id, idToken, err := s.cfg.SSO.Exchange(r.Context(), sso.Login{State: l.State, Nonce: l.Nonce, Verifier: l.Verifier}, q.Get("code"))
	if err != nil {
		s.log.Error("complete SSO sign-in", "error", err)
		s.loginError(w, r, http.StatusUnauthorized, l.Next, "The sign-in failed. Try again.")
		return
	}
	u, err := s.svc.SignInIdentity(r.Context(), identityOf(s.cfg.SSO, id), "web")
	switch {
	case errors.Is(err, core.ErrNoVerifiedEmail):
		s.loginError(w, r, http.StatusForbidden, "/", "Your account at the identity provider has no verified email address.")
		return
	case errors.Is(err, domain.ErrUnauthenticated):
		s.loginError(w, r, http.StatusForbidden, "/", "Your account is disabled. Ask an administrator.")
		return
	case errors.Is(err, domain.ErrConflict):
		s.log.Warn("SSO sign-in conflict", "error", err)
		s.loginError(w, r, http.StatusConflict, "/", "Your email address belongs to a different account. Ask an administrator.")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	token, _, err := s.svc.NewSession(r.Context(), u, idToken)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("signed in", "user", u.Email, "ip", s.clientIP(r), "method", "sso")
	s.setSession(w, token)
	http.Redirect(w, r, l.Next, http.StatusSeeOther)
}

// identityOf changes a provider identity into a core identity.
func identityOf(c *sso.Client, id *sso.Identity) core.Identity {
	out := core.Identity{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email, EmailVerified: id.EmailVerified, Name: id.Name}
	if admin, managed := c.Config().Admin(id); managed {
		out.Admin = &admin
	}
	return out
}
