// Package api is the JSON API of Squared, version 1. It uses the same
// core.Service as the web interface. Clients authenticate with a personal
// access token in the header "Authorization: Bearer <token>".
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"squared/internal/core"
	"squared/internal/domain"
)

// Server serves the API.
type Server struct {
	svc *core.Service
	log *slog.Logger
}

// New returns a Server.
func New(svc *core.Service, log *slog.Logger) *Server {
	return &Server{svc: svc, log: log}
}

// Routes registers the routes on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	const p = "/api/v1"
	h := func(pattern string, fn handlerFunc) { mux.Handle(pattern, s.auth(fn)) }

	h("GET "+p+"/me", s.me)
	h("GET "+p+"/packages", s.listPackages)
	h("GET "+p+"/packages/{name}", s.packageVersions)
	h("GET "+p+"/packages/{name}/{version}", s.getPackage)

	h("GET "+p+"/users", s.listUsers)
	h("POST "+p+"/users", s.createUser)

	h("GET "+p+"/tenants", s.listTenants)
	h("POST "+p+"/tenants", s.createTenant)
	h("GET "+p+"/tenants/{tenant}", s.getTenant)
	h("PUT "+p+"/tenants/{tenant}", s.updateTenant)
	h("DELETE "+p+"/tenants/{tenant}", s.deleteTenant)
	h("POST "+p+"/tenants/{tenant}/sync", s.syncTenant)
	h("GET "+p+"/tenants/{tenant}/audit", s.listAudit)

	h("GET "+p+"/tenants/{tenant}/members", s.listMembers)
	h("PUT "+p+"/tenants/{tenant}/members", s.putMember)
	h("DELETE "+p+"/tenants/{tenant}/members/{user}", s.removeMember)

	h("GET "+p+"/tenants/{tenant}/environments", s.listEnvironments)
	h("POST "+p+"/tenants/{tenant}/environments", s.createEnvironment)
	h("DELETE "+p+"/tenants/{tenant}/environments/{env}", s.deleteEnvironment)

	h("GET "+p+"/tenants/{tenant}/deployments", s.listDeployments)
	h("POST "+p+"/tenants/{tenant}/deployments", s.createDeployment)
	h("GET "+p+"/tenants/{tenant}/deployments/{env}/{name}", s.getDeployment)
	h("PUT "+p+"/tenants/{tenant}/deployments/{env}/{name}", s.updateDeployment)
	h("DELETE "+p+"/tenants/{tenant}/deployments/{env}/{name}", s.deleteDeployment)
	h("POST "+p+"/tenants/{tenant}/deployments/{env}/{name}/rollback", s.rollback)
	h("POST "+p+"/tenants/{tenant}/deployments/{env}/{name}/sync", s.syncDeployment)

	mux.Handle(p+"/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint", nil)
	}))
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, u *domain.User) error

type ctxKey struct{}

// auth checks the bearer token and calls fn with the user.
func (s *Server) auth(fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="squared"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "an API token is necessary", nil)
			return
		}
		u, err := s.svc.TokenUser(r.Context(), strings.TrimSpace(token))
		if err != nil {
			if !errors.Is(err, domain.ErrUnauthenticated) {
				s.log.Error("check API token", "error", err)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="squared", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "the API token is not valid", nil)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, u)
		if err := fn(w, r.WithContext(ctx), u); err != nil {
			s.fail(w, r, err)
		}
	})
}

// fail writes an error response that agrees with err.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *domain.ValidationError
	var serr *syntaxError
	switch {
	case errors.As(err, &serr):
		writeError(w, http.StatusBadRequest, "bad_request", serr.Error(), nil)
	case errors.As(err, &verr):
		writeError(w, http.StatusUnprocessableEntity, "invalid", "the input is not valid", verr.Fields)
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "the object does not exist, or you do not have access to it", nil)
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "you do not have the permission for this operation", nil)
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, domain.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "an API token is necessary", nil)
	default:
		s.log.Error("API request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "an internal error occurred", nil)
	}
}

// Error is the body of all error responses.
type Error struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes an error.
type ErrorBody struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Fields  []domain.FieldError `json:"fields,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string, fields []domain.FieldError) {
	writeJSON(w, status, Error{Error: ErrorBody{Code: code, Message: msg, Fields: fields}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"encode response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

type syntaxError struct{ msg string }

func (e *syntaxError) Error() string { return e.msg }

// decode reads a JSON request body into v. It rejects unknown fields, so
// that a typing error in a field name does not go unnoticed.
func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return &syntaxError{"the content type must be application/json"}
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return &syntaxError{"the request body is empty"}
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &syntaxError{"the request body is too large"}
		}
		return &syntaxError{fmt.Sprintf("the request body is not valid JSON: %v", err)}
	}
	if dec.More() {
		return &syntaxError{"the request body has more than one JSON value"}
	}
	return nil
}
