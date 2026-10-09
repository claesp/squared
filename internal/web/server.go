// Package web is the browser interface of Squared. It makes HTML on the
// server and uses no JavaScript. Forms send POST requests, and the server
// answers with a redirect (the POST/redirect/GET pattern).
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"squared/internal/core"
	"squared/internal/domain"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Cookie names.
const (
	flashCookie = "squared_flash"
)

// Config is the configuration of the web interface.
type Config struct {
	// CookieSecure sets the Secure attribute on cookies. Set it to false
	// only for local tests over plain HTTP on a host other than localhost.
	CookieSecure bool
	// RegistryHost is the host name of Harbor for image push instructions.
	RegistryHost string
	// ProjectPrefix is the prefix of the Harbor project of a tenant.
	ProjectPrefix string
	// GitURL is the public URL of Forgejo, for links to commits. Optional.
	GitURL string
	// GitOrg is the Forgejo organization of the repositories.
	GitOrg string
	// TrustProxy tells that a reverse proxy sets X-Forwarded-For.
	TrustProxy bool
}

// Server serves the web interface.
type Server struct {
	svc     *core.Service
	cfg     Config
	log     *slog.Logger
	pages   map[string]*template.Template
	limiter *limiter
}

// New returns a Server.
func New(svc *core.Service, cfg Config, log *slog.Logger) (*Server, error) {
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{
		svc:     svc,
		cfg:     cfg,
		log:     log,
		pages:   pages,
		limiter: newLimiter(10, 5*time.Minute),
	}, nil
}

func (s *Server) sessionCookieName() string {
	// The "__Host-" prefix makes the browser accept the cookie only if it
	// is Secure, has no Domain and has Path "/".
	if s.cfg.CookieSecure {
		return "__Host-squared_session"
	}
	return "squared_session"
}

// Routes registers the routes on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // The embedded directory always exists.
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("POST /logout", s.user(s.logout))

	mux.Handle("GET /{$}", s.user(s.dashboard))
	mux.Handle("GET /catalog", s.user(s.catalogPage))
	mux.Handle("GET /catalog/{name}", s.user(s.packagePage))

	mux.Handle("GET /account", s.user(s.accountPage))
	mux.Handle("POST /account/password", s.user(s.changePassword))
	mux.Handle("POST /account/tokens", s.user(s.createToken))
	mux.Handle("POST /account/tokens/{id}/delete", s.user(s.deleteToken))

	mux.Handle("GET /admin/users", s.user(s.usersPage))
	mux.Handle("POST /admin/users", s.user(s.createUser))
	mux.Handle("POST /admin/users/{id}/disable", s.user(s.setUserDisabled(true)))
	mux.Handle("POST /admin/users/{id}/enable", s.user(s.setUserDisabled(false)))
	mux.Handle("POST /admin/users/{id}/password", s.user(s.resetPassword))
	mux.Handle("GET /admin/tenants/new", s.user(s.newTenantPage))
	mux.Handle("POST /admin/tenants", s.user(s.createTenant))
	mux.Handle("GET /admin/audit", s.user(s.platformAuditPage))

	mux.Handle("GET /tenants/{tenant}", s.user(s.tenantPage))
	mux.Handle("POST /tenants/{tenant}/sync", s.user(s.syncTenant))
	mux.Handle("GET /tenants/{tenant}/settings", s.user(s.tenantSettingsPage))
	mux.Handle("POST /tenants/{tenant}/settings", s.user(s.updateTenant))
	mux.Handle("GET /tenants/{tenant}/members", s.user(s.membersPage))
	mux.Handle("POST /tenants/{tenant}/members", s.user(s.putMember))
	mux.Handle("POST /tenants/{tenant}/members/{user}/remove", s.user(s.removeMember))
	mux.Handle("POST /tenants/{tenant}/environments", s.user(s.createEnvironment))
	mux.Handle("POST /tenants/{tenant}/environments/{env}/delete", s.user(s.deleteEnvironment))
	mux.Handle("GET /tenants/{tenant}/audit", s.user(s.auditPage))
	mux.Handle("GET /tenants/{tenant}/new", s.user(s.choosePackagePage))
	mux.Handle("GET /tenants/{tenant}/new/{package}", s.user(s.newDeploymentPage))
	mux.Handle("POST /tenants/{tenant}/new/{package}", s.user(s.createDeployment))
	mux.Handle("GET /tenants/{tenant}/deployments/{env}/{name}", s.user(s.deploymentPage))
	mux.Handle("GET /tenants/{tenant}/deployments/{env}/{name}/edit", s.user(s.editDeploymentPage))
	mux.Handle("POST /tenants/{tenant}/deployments/{env}/{name}/edit", s.user(s.updateDeployment))
	mux.Handle("POST /tenants/{tenant}/deployments/{env}/{name}/rollback", s.user(s.rollback))
	mux.Handle("POST /tenants/{tenant}/deployments/{env}/{name}/sync", s.user(s.syncDeployment))
	mux.Handle("POST /tenants/{tenant}/deployments/{env}/{name}/delete", s.user(s.deleteDeployment))
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
)

func userFrom(r *http.Request) *domain.User {
	u, _ := r.Context().Value(userKey).(*domain.User)
	return u
}

func sessionFrom(r *http.Request) *domain.Session {
	s, _ := r.Context().Value(sessionKey).(*domain.Session)
	return s
}

// handlerFunc is a handler that can return an error. The error becomes an
// error page.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// user wraps a handler that needs a signed-in user. For POST requests it
// also checks the CSRF token.
func (s *Server) user(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.sessionCookieName())
		if err != nil {
			s.redirectToLogin(w, r)
			return
		}
		u, sess, err := s.svc.SessionUser(r.Context(), c.Value)
		if errors.Is(err, domain.ErrUnauthenticated) {
			s.clearSession(w)
			s.redirectToLogin(w, r)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if r.Method == http.MethodPost {
			got := r.PostFormValue("csrf")
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
				http.Error(w, "The form is too old or not from this site. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, sessionKey, sess)
		if err := h(w, r.WithContext(ctx)); err != nil {
			s.fail(w, r.WithContext(ctx), err)
		}
	})
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
}

// fail shows an error page with a status that agrees with the error.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "An internal error occurred. The problem is recorded in the log."
	switch {
	case errors.Is(err, domain.ErrNotFound):
		status, msg = http.StatusNotFound, "The page does not exist, or you do not have access to it."
	case errors.Is(err, domain.ErrForbidden):
		status, msg = http.StatusForbidden, "You do not have the permission for this operation."
	case errors.Is(err, domain.ErrConflict):
		status, msg = http.StatusConflict, "The operation is not possible now: "+err.Error()
	case errors.Is(err, domain.ErrUnauthenticated):
		s.redirectToLogin(w, r)
		return
	}
	var verr *domain.ValidationError
	if errors.As(err, &verr) {
		status, msg = http.StatusUnprocessableEntity, verr.Error()
	}
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	w.WriteHeader(status)
	s.renderStatus(w, r, status, "error", page{Title: http.StatusText(status), Data: msg})
}

// safeNext returns next if it is a path on this site, otherwise "/". This
// prevents open redirects.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// setFlash stores a short message for the next page.
func (s *Server) setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	msg, err := url.QueryUnescape(c.Value)
	if err != nil || len(msg) > 300 {
		return ""
	}
	return msg
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, flash string) error {
	if flash != "" {
		s.setFlash(w, flash)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookieName(), Value: token, Path: "/",
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookieName(), Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// clientIP returns the address of the client for rate limits.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
