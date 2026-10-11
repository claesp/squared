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
// sign-in, registration and webhook settings of app.ini, which the guard
// checks. It never serves secrets (see forgejo.ConfigReporter).
func runForgejoConfig(ctx context.Context, log *slog.Logger) error {
	var env config.Reader
	path := env.String("SQUARED_FORGEJO_APP_INI", "/data/gitea/conf/app.ini")
	listen := env.String("SQUARED_LISTEN", ":8083")
	if err := env.Err(); err != nil {
		return err
	}
	reporter := forgejo.NewConfigReporter(path)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("GET /settings", reporter)
	log.Info("forgejo-config started", "file", path, "listen", listen)
	srv := httpx.NewServer(listen, httpx.Log(log, nil, mux))
	if err := httpx.Serve(ctx, srv, log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
