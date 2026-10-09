// Package httpx has HTTP middleware that all Squared servers use.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"squared/internal/metrics"
)

// SecurityHeaders adds headers that protect browsers. The content
// security policy blocks all scripts, because the web interface uses no
// JavaScript. formOrigins are other origins that forms can lead to: the
// browser applies "form-action" also to the redirect after a form, for
// example the sign-out at the identity provider.
func SecurityHeaders(hsts bool, formOrigins []string, next http.Handler) http.Handler {
	formAction := "'self'"
	for _, o := range formOrigins {
		formAction += " " + o
	}
	csp := "default-src 'none'; style-src 'self'; img-src 'self'; form-action " + formAction + "; frame-ancestors 'none'; base-uri 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// LimitBody limits the size of request bodies.
func LimitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Log writes one log line for each request and changes a panic into a
// status 500 response. If reg is not nil, Log also counts the requests.
func Log(log *slog.Logger, reg *metrics.Registry, next http.Handler) http.Handler {
	var requests *metrics.Counter
	var durations *metrics.Summary
	if reg != nil {
		requests = reg.NewCounter("squared_http_requests_total", "HTTP requests by route and status class.", "method", "route", "status")
		durations = reg.NewSummary("squared_http_request_seconds", "Duration of HTTP requests by route.", "route")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic in handler", "panic", v, "method", r.Method, "path", r.URL.Path)
				if sw.status == 0 {
					http.Error(sw, "internal error", http.StatusInternalServerError)
				}
			}
			if sw.status == 0 {
				sw.status = http.StatusOK // The handler wrote nothing.
			}
			if requests != nil {
				// The mux sets the pattern. A route has few values, a path
				// has many. Unknown paths have no pattern.
				route := r.Pattern
				if route == "" {
					route = "none"
				}
				requests.Inc(r.Method, route, fmt.Sprintf("%dxx", sw.status/100))
				durations.Observe(time.Since(start).Seconds(), route)
			}
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
				return
			}
			log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(sw, r)
	})
}

// Serve runs srv until ctx is done, then stops it gracefully.
func Serve(ctx context.Context, srv *http.Server, log *slog.Logger) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String())
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NewServer returns an HTTP server with safe timeouts.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
