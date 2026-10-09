package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"squared/internal/config"
	"squared/internal/forgejo"
	"squared/internal/gitops"
	"squared/internal/harbor"
	"squared/internal/httpx"
	"squared/internal/kube"
	"squared/internal/queue"
	"squared/internal/rest"
	"squared/internal/worker"
)

func runWorker(ctx context.Context, log *slog.Logger) error {
	var env config.Reader
	amqpURL := env.Secret("SQUARED_AMQP_URL", true)
	forgejoURL := env.Required("SQUARED_FORGEJO_URL")
	forgejoToken := env.Secret("SQUARED_FORGEJO_TOKEN", true)
	harborURL := env.Required("SQUARED_HARBOR_URL")
	harborUser := env.String("SQUARED_HARBOR_USER", "admin")
	harborPassword := env.Secret("SQUARED_HARBOR_PASSWORD", true)
	healthListen := env.String("SQUARED_HEALTH_LISTEN", ":8081")
	statusInterval := env.Duration("SQUARED_STATUS_INTERVAL", 10*time.Second)
	consumerCfg := queue.ConsumerConfig{
		URL:         amqpURL,
		Concurrency: env.Int("SQUARED_WORKER_CONCURRENCY", 4),
		MaxAttempts: env.Int("SQUARED_WORKER_MAX_ATTEMPTS", 15),
	}
	cfg := worker.Config{
		GitOps: gitops.Config{
			Org:                  env.String("SQUARED_GIT_ORG", "squared"),
			PlatformRepo:         env.String("SQUARED_PLATFORM_REPO", "platform"),
			Branch:               env.String("SQUARED_GIT_BRANCH", "main"),
			RepoURLBase:          env.String("SQUARED_ARGOCD_REPO_URL", forgejoURL),
			ArgoNamespace:        env.String("SQUARED_ARGOCD_NAMESPACE", "argocd"),
			ClusterServer:        env.String("SQUARED_CLUSTER_SERVER", "https://kubernetes.default.svc"),
			WorkerServiceAccount: env.String("SQUARED_WORKER_SERVICE_ACCOUNT", "squared-worker"),
			WorkerNamespace:      env.String("SQUARED_WORKER_NAMESPACE", env.String("POD_NAMESPACE", "squared")),
			IngressNamespace:     env.String("SQUARED_INGRESS_NAMESPACE", ""),
		},
		RegistryHost:    env.Required("SQUARED_REGISTRY_HOST"),
		ProjectPrefix:   env.String("SQUARED_PROJECT_PREFIX", ""),
		PlatformProject: env.String("SQUARED_PLATFORM_PROJECT", "platform"),
		IngressClass:    env.String("SQUARED_INGRESS_CLASS", ""),
		IngressTLS:      env.Bool("SQUARED_INGRESS_TLS", false),
	}
	kubeCfg, kubeErr := kubeConfig(&env)
	st, err := openStore(ctx, &env)
	if err != nil {
		return err
	}
	defer st.Close()
	if kubeErr != nil {
		return kubeErr
	}
	kc, err := kube.New(kubeCfg)
	if err != nil {
		return err
	}

	httpClient := rest.NewHTTPClient(nil)
	fj := forgejo.New(forgejoURL, forgejoToken, httpClient)
	hb := harbor.New(harborURL, harborUser, harborPassword, httpClient)
	w := worker.New(cfg, st, fj, hb, kc, log)

	// The shared objects are necessary before the first task. Retry until
	// Forgejo and Harbor are available.
	for attempt := 1; ; attempt++ {
		err := w.Prepare(ctx)
		if err == nil {
			break
		}
		delay := min(time.Duration(attempt)*2*time.Second, 30*time.Second)
		log.Warn("prepare shared objects; retry later", "error", err, "delay", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	log.Info("shared objects are ready")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, r *http.Request) {
		if err := st.Ping(r.Context()); err != nil {
			http.Error(rw, "database: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = rw.Write([]byte("ok\n"))
	})

	var wg sync.WaitGroup
	defer wg.Wait()
	consumer := queue.NewConsumer(consumerCfg, w, log)
	wg.Go(func() {
		if err := consumer.Run(ctx); err != nil {
			log.Error("consumer stopped", "error", err)
		}
	})
	wg.Go(func() { w.WatchStatus(ctx, statusInterval) })

	srv := httpx.NewServer(healthListen, httpx.Log(log, mux))
	if err := httpx.Serve(ctx, srv, log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// kubeConfig returns the in-cluster configuration, or the configuration
// from SQUARED_KUBE_API_URL, SQUARED_KUBE_TOKEN_FILE and
// SQUARED_KUBE_CA_FILE for development outside a cluster.
func kubeConfig(env *config.Reader) (kube.Config, error) {
	if server := env.String("SQUARED_KUBE_API_URL", ""); server != "" {
		cfg := kube.Config{
			Server:    server,
			TokenFile: env.Required("SQUARED_KUBE_TOKEN_FILE"),
			CAFile:    env.Required("SQUARED_KUBE_CA_FILE"),
		}
		return cfg, env.Err()
	}
	cfg, err := kube.InClusterConfig()
	if err != nil {
		return kube.Config{}, fmt.Errorf("kubernetes: %w; set SQUARED_KUBE_API_URL outside a cluster", err)
	}
	return cfg, env.Err()
}
