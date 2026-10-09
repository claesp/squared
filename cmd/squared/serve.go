package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"squared/internal/api"
	"squared/internal/catalog"
	"squared/internal/config"
	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/httpx"
	"squared/internal/queue"
	"squared/internal/render"
	"squared/internal/store"
	"squared/internal/web"
)

func openStore(ctx context.Context, env *config.Reader) (*store.Store, error) {
	url := env.Secret("SQUARED_DATABASE_URL", true)
	if err := env.Err(); err != nil {
		return nil, err
	}
	return store.Open(ctx, url)
}

func migrate(ctx context.Context, log *slog.Logger) error {
	var env config.Reader
	st, err := openStore(ctx, &env)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Migrate(ctx, log)
}

func serve(ctx context.Context, log *slog.Logger) error {
	var env config.Reader
	listen := env.String("SQUARED_LISTEN", ":8080")
	amqpURL := env.Secret("SQUARED_AMQP_URL", true)
	catalogDir := env.String("SQUARED_CATALOG_DIR", "/etc/squared/catalog")
	runMigrations := env.Bool("SQUARED_MIGRATE", true)
	webCfg := web.Config{
		CookieSecure:  env.Bool("SQUARED_COOKIE_SECURE", true),
		RegistryHost:  env.Required("SQUARED_REGISTRY_HOST"),
		ProjectPrefix: env.String("SQUARED_PROJECT_PREFIX", ""),
		GitURL:        env.String("SQUARED_GIT_PUBLIC_URL", ""),
		GitOrg:        env.String("SQUARED_GIT_ORG", "squared"),
		TrustProxy:    env.Bool("SQUARED_TRUST_PROXY", false),
	}
	hsts := env.Bool("SQUARED_HSTS", false)
	coreCfg := core.Config{
		SessionTTL: env.Duration("SQUARED_SESSION_TTL", 12*time.Hour),
		DefaultQuota: domain.Quota{
			CPU:    env.String("SQUARED_DEFAULT_QUOTA_CPU", "4"),
			Memory: env.String("SQUARED_DEFAULT_QUOTA_MEMORY", "8Gi"),
			Pods:   env.Int("SQUARED_DEFAULT_QUOTA_PODS", 50),
		},
	}
	adminEmail := env.String("SQUARED_BOOTSTRAP_ADMIN_EMAIL", "")
	adminPassword := env.Secret("SQUARED_BOOTSTRAP_ADMIN_PASSWORD", adminEmail != "")
	st, err := openStore(ctx, &env)
	if err != nil {
		return err
	}
	defer st.Close()

	if runMigrations {
		if err := st.Migrate(ctx, log); err != nil {
			return err
		}
	}
	cat, err := catalog.Load(os.DirFS(catalogDir), render.Check)
	if err != nil {
		return err
	}
	log.Info("catalog loaded", "dir", catalogDir, "packages", len(cat.Latest()))

	svc := core.New(st, cat, render.Check, log, coreCfg)
	if adminEmail != "" {
		created, err := svc.BootstrapAdmin(ctx, adminEmail, adminPassword)
		if err != nil {
			return fmt.Errorf("bootstrap administrator: %w", err)
		}
		if created {
			log.Info("first administrator added", "email", adminEmail)
		}
	}

	pub := queue.NewPublisher(amqpURL, log)
	defer pub.Close()

	webServer, err := web.New(svc, webCfg, log)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	webServer.Routes(mux)
	api.New(svc, log).Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := svc.Ping(ctx); err != nil {
			http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})

	// Cross-origin protection rejects state-changing requests from other
	// sites (it uses the Sec-Fetch-Site and Origin headers). The forms
	// also have CSRF tokens, and the API uses bearer tokens, not cookies.
	cop := http.NewCrossOriginProtection()
	var handler http.Handler = mux
	handler = cop.Handler(handler)
	handler = httpx.LimitBody(1<<20, handler)
	handler = httpx.SecurityHeaders(hsts, handler)
	handler = httpx.Log(log, handler)

	var wg sync.WaitGroup
	defer wg.Wait()
	relay := &queue.Relay{Outbox: st, Publisher: pub, Log: log}
	wg.Go(func() { relay.Run(ctx) })
	wg.Go(func() { cleanUp(ctx, svc, log) })

	srv := httpx.NewServer(listen, handler)
	if err := httpx.Serve(ctx, srv, log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cleanUp removes expired sessions and old outbox rows each hour.
func cleanUp(ctx context.Context, svc *core.Service, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := svc.CleanUp(ctx); err != nil && ctx.Err() == nil {
			log.Error("clean up", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
