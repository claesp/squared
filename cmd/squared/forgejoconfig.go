package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"squared/internal/config"
	"squared/internal/forgejo"
	"squared/internal/httpx"
)

// runForgejoConfig runs in the pod of Forgejo as a sidecar. It serves the
// section [oauth2_client] of app.ini, which the guard checks. The section
// has no secrets; the role never serves other sections.
func runForgejoConfig(ctx context.Context, log *slog.Logger) error {
	var env config.Reader
	path := env.String("SQUARED_FORGEJO_APP_INI", "/data/gitea/conf/app.ini")
	listen := env.String("SQUARED_LISTEN", ":8083")
	if err := env.Err(); err != nil {
		return err
	}
	reporter := forgejo.NewSectionReporter(path, "oauth2_client")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("GET /oauth2-client", reporter)
	log.Info("forgejo-config started", "file", path, "listen", listen)
	srv := httpx.NewServer(listen, httpx.Log(log, nil, mux))
	if err := httpx.Serve(ctx, srv, log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
