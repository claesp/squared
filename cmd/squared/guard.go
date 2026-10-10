package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"squared/internal/config"
	"squared/internal/guard"
	"squared/internal/httpx"
	"squared/internal/keycloak"
	"squared/internal/kube"
	"squared/internal/metrics"
	"squared/internal/rest"
)

// runGuard protects the identity provider groups that the service
// configurations give rights to. See package guard.
func runGuard(ctx context.Context, log *slog.Logger) error {
	var env config.Reader
	configFile := env.String("SQUARED_GUARD_CONFIG_FILE", "/etc/squared/guard/guard.json")
	interval := env.Duration("SQUARED_GUARD_INTERVAL", 30*time.Second)
	healthListen := env.String("SQUARED_HEALTH_LISTEN", ":8082")
	keycloakURL := env.Required("SQUARED_KEYCLOAK_URL")
	realm := env.Required("SQUARED_KEYCLOAK_REALM")
	clientID := env.Required("SQUARED_KEYCLOAK_CLIENT_ID")
	clientSecret := env.Secret("SQUARED_KEYCLOAK_CLIENT_SECRET", true)
	kubeCfg, err := kubeConfig(&env)
	if err != nil {
		return err
	}
	fileCfg, err := guard.LoadFile(configFile)
	if err != nil {
		return err
	}
	kc, err := kube.New(kubeCfg)
	if err != nil {
		return err
	}
	sources, err := guard.BuildSources(fileCfg, kc)
	if err != nil {
		return err
	}
	reg := &metrics.Registry{}
	g := guard.New(guard.Config{Permission: fileCfg.Permission, Groups: fileCfg.Groups}, sources,
		keycloak.New(keycloakURL, realm, clientID, clientSecret, rest.NewHTTPClient(nil)), log, reg)
	log.Info("guard started", "sources", len(sources), "interval", interval.String())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("GET /status", g.StatusHandler())
	mux.Handle("GET /metrics", reg.Handler())
	go g.Run(ctx, interval)
	srv := httpx.NewServer(healthListen, httpx.Log(log, nil, mux))
	if err := httpx.Serve(ctx, srv, log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
