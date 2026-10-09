package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/domain"
)

// page is the data of all templates.
type page struct {
	Title   string
	User    *domain.User
	CSRF    string
	Flash   string
	Nav     string // Active item of the main navigation.
	Refresh int    // If more than 0, the browser loads the page again after this number of seconds.
	Errors  *domain.ValidationError
	Form    map[string]string // Values for form fields after a validation error.
	Access  *core.Access      // Set on tenant pages.
	Tab     string            // Active tab on tenant pages.
	Data    any
}

// FieldError returns the error message for a form field.
func (p page) FieldError(field string) string {
	if p.Errors == nil {
		return ""
	}
	return p.Errors.FieldMessage(field)
}

// Value returns the value of a form field after a validation error.
func (p page) Value(field string) string { return p.Form[field] }

// staticPath returns the path of an embedded static file with a hash of
// its content. A changed file thus gets a new address, and browsers do not
// use an old copy from their cache.
func staticPath(name string) string {
	b, err := fs.ReadFile(staticFS, "static/"+name)
	if err != nil {
		return "/static/" + name
	}
	sum := sha256.Sum256(b)
	return "/static/" + name + "?v=" + hex.EncodeToString(sum[:6])
}

var funcs = template.FuncMap{
	"static": staticPath,
	"statusClass": func(s domain.Status) string {
		switch s {
		case domain.StatusReady, domain.StatusDeployed:
			return "ok"
		case domain.StatusFailed, domain.StatusDegraded:
			return "bad"
		case domain.StatusDeleting:
			return "warn"
		}
		return "busy"
	},
	"time": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"timeptr": func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"short": func(s string) string {
		if len(s) > 10 {
			return s[:10]
		}
		return s
	},
	"formValue": catalog.FormValue,
	"can": func(a *core.Access, role string) bool {
		return a != nil && a.Can(domain.Role(role))
	},
	"json": func(v any) string {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err.Error()
		}
		return string(b)
	},
	"sortedKeys": func(m map[string]any) []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	},
	"display": func(v any) string {
		switch x := v.(type) {
		case nil:
			return "(not set)"
		case map[string]any:
			if len(x) == 0 {
				return "(none)"
			}
			return strings.TrimSpace(catalog.FormValue(x))
		}
		return catalog.FormValue(v)
	},
	"roles": func() []domain.Role { return domain.Roles },
}

func parseTemplates() (map[string]*template.Template, error) {
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, name := range names {
		base := strings.TrimSuffix(path.Base(name), ".html")
		if base == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", name)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		pages[base] = t
	}
	if len(pages) == 0 {
		return nil, errors.New("no templates")
	}
	return pages, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) error {
	return s.renderStatus(w, r, http.StatusOK, name, p)
}

// renderStatus writes a page. The status must already be written if it is
// not 200. The page is made in a buffer first, thus a template error does
// not give a half page.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, p page) error {
	t, ok := s.pages[name]
	if !ok {
		return fmt.Errorf("no template %q", name)
	}
	if p.User == nil {
		p.User = userFrom(r)
	}
	if sess := sessionFrom(r); sess != nil {
		p.CSRF = sess.CSRFToken
	}
	if p.Flash == "" && status == http.StatusOK {
		p.Flash = s.takeFlash(w, r)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		return fmt.Errorf("render %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, err := buf.WriteTo(w)
	return err
}

// invalid shows a form again with the validation errors. It returns other
// errors without change.
func (s *Server) invalid(w http.ResponseWriter, r *http.Request, err error, name string, p page) error {
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		return err
	}
	p.Errors = verr
	if p.Form == nil {
		p.Form = formMap(r)
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	return s.renderStatus(w, r, http.StatusUnprocessableEntity, name, p)
}

// formMap returns the first value of each form field, without secrets.
func formMap(r *http.Request) map[string]string {
	m := map[string]string{}
	for k, v := range r.PostForm {
		if k == "csrf" || strings.Contains(k, "password") || len(v) == 0 {
			continue
		}
		m[k] = v[0]
	}
	return m
}
