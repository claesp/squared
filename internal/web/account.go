package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"squared/internal/core"
	"squared/internal/domain"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if err := s.render(w, r, "login", page{Title: "Sign in", Form: map[string]string{"next": safeNext(r.URL.Query().Get("next"))}}); err != nil {
		s.fail(w, r, err)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	form := map[string]string{"email": email, "next": next}

	ip := s.clientIP(r)
	now := time.Now()
	// Count the attempts for the address and for the account. Both
	// limits are necessary: one client can try many accounts, and many
	// clients can try one account.
	if !s.limiter.allow("ip:"+ip, now) || !s.limiter.allow("email:"+email, now) {
		s.log.Warn("sign-in rate limit", "ip", ip, "email", email)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = s.renderStatus(w, r, http.StatusTooManyRequests, "login", page{Title: "Sign in", Form: form,
			Errors: &domain.ValidationError{Fields: []domain.FieldError{{Field: "form", Message: "Too many attempts. Wait some minutes and try again."}}}})
		return
	}
	u, err := s.svc.Authenticate(r.Context(), email, password)
	if errors.Is(err, domain.ErrUnauthenticated) {
		s.log.Info("sign-in failed", "ip", ip, "email", email)
		w.WriteHeader(http.StatusUnauthorized)
		_ = s.renderStatus(w, r, http.StatusUnauthorized, "login", page{Title: "Sign in", Form: form,
			Errors: &domain.ValidationError{Fields: []domain.FieldError{{Field: "form", Message: "The email address or the password is not correct."}}}})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.limiter.reset("email:" + email)
	token, _, err := s.svc.NewSession(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("signed in", "user", u.Email, "ip", ip)
	s.setSession(w, token)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(s.sessionCookieName()); err == nil {
		if err := s.svc.EndSession(r.Context(), c.Value); err != nil {
			return err
		}
	}
	s.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return nil
}

type accountData struct {
	Tokens   []domain.APIToken
	NewToken string
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) error {
	return s.showAccount(w, r, "", nil)
}

func (s *Server) showAccount(w http.ResponseWriter, r *http.Request, newToken string, formErr error) error {
	tokens, err := s.svc.ListAPITokens(r.Context(), userFrom(r))
	if err != nil {
		return err
	}
	p := page{Title: "Account", Nav: "account", Data: accountData{Tokens: tokens, NewToken: newToken}}
	if formErr != nil {
		return s.invalid(w, r, formErr, "account", p)
	}
	return s.render(w, r, "account", p)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	u := userFrom(r)
	if r.PostFormValue("password") != r.PostFormValue("password2") {
		return s.showAccount(w, r, "", domain.Invalid("password2", "the two new passwords are not equal"))
	}
	err := s.svc.ChangePassword(r.Context(), u, r.PostFormValue("current_password"), r.PostFormValue("password"))
	if err != nil {
		return s.showAccount(w, r, "", err)
	}
	// All sessions ended. Make a new session for this browser.
	token, _, err := s.svc.NewSession(r.Context(), u)
	if err != nil {
		return err
	}
	s.setSession(w, token)
	return s.redirect(w, r, "/account", "Your password is changed. Other sessions are signed out.")
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	days, err := strconv.Atoi(r.PostFormValue("days"))
	if err != nil {
		return s.showAccount(w, r, "", domain.Invalid("days", "must be a number"))
	}
	token, _, err := s.svc.CreateAPIToken(r.Context(), userFrom(r), strings.TrimSpace(r.PostFormValue("name")), days)
	if err != nil {
		return s.showAccount(w, r, "", err)
	}
	// Show the token one time. The page is not cached (Cache-Control: no-store).
	return s.showAccount(w, r, token, nil)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) error {
	if err := s.svc.DeleteAPIToken(r.Context(), userFrom(r), r.PathValue("id")); err != nil {
		return err
	}
	return s.redirect(w, r, "/account", "The token is removed.")
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	tenants, err := s.svc.ListTenants(r.Context(), userFrom(r))
	if err != nil {
		return err
	}
	refresh := 0
	for _, t := range tenants {
		if !t.Status.Done() {
			refresh = 5
		}
	}
	return s.render(w, r, "dashboard", page{Title: "Tenants", Nav: "tenants", Data: tenants, Refresh: refresh})
}

func (s *Server) catalogPage(w http.ResponseWriter, r *http.Request) error {
	return s.render(w, r, "catalog", page{Title: "Catalog", Nav: "catalog", Data: s.svc.Catalog().Latest()})
}

func (s *Server) packagePage(w http.ResponseWriter, r *http.Request) error {
	versions := s.svc.Catalog().Versions(r.PathValue("name"))
	if len(versions) == 0 {
		return domain.ErrNotFound
	}
	return s.render(w, r, "package", page{Title: versions[0].Title, Nav: "catalog", Data: versions})
}

type usersData struct {
	Users []domain.User
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) error {
	return s.showUsers(w, r, nil)
}

func (s *Server) showUsers(w http.ResponseWriter, r *http.Request, formErr error) error {
	users, err := s.svc.ListUsers(r.Context(), userFrom(r))
	if err != nil {
		return err
	}
	p := page{Title: "Users", Nav: "admin", Data: usersData{Users: users}}
	if formErr != nil {
		return s.invalid(w, r, formErr, "users", p)
	}
	return s.render(w, r, "users", p)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) error {
	_, err := s.svc.CreateUser(r.Context(), userFrom(r), core.NewUser{
		Email:    r.PostFormValue("email"),
		Name:     r.PostFormValue("name"),
		Password: r.PostFormValue("password"),
		Admin:    r.PostFormValue("admin") == "on",
	})
	if err != nil {
		return s.showUsers(w, r, err)
	}
	return s.redirect(w, r, "/admin/users", "The user is added.")
}

func (s *Server) setUserDisabled(disabled bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := s.svc.SetUserDisabled(r.Context(), userFrom(r), r.PathValue("id"), disabled); err != nil {
			return s.showUsers(w, r, err)
		}
		msg := "The user is enabled."
		if disabled {
			msg = "The user is disabled and signed out."
		}
		return s.redirect(w, r, "/admin/users", msg)
	}
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) error {
	if err := s.svc.ResetPassword(r.Context(), userFrom(r), r.PathValue("id"), r.PostFormValue("password")); err != nil {
		return s.showUsers(w, r, err)
	}
	return s.redirect(w, r, "/admin/users", "The password is changed and the user is signed out.")
}

func (s *Server) platformAuditPage(w http.ResponseWriter, r *http.Request) error {
	events, err := s.svc.ListPlatformAudit(r.Context(), userFrom(r), 200)
	if err != nil {
		return err
	}
	return s.render(w, r, "audit", page{Title: "Audit log", Nav: "admin", Data: events})
}
