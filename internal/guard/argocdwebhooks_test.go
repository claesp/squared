package guard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeWebhook(t *testing.T) {
	// A webhook with a secret for GitLab only, as Argo CD answers.
	var headers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/webhook" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		switch {
		case r.Header.Get("X-GitHub-Event") == "push":
			headers = append(headers, "github")
		case r.Header.Get("X-Gitlab-Event") == "Push Hook":
			headers = append(headers, "gitlab")
			http.Error(w, "Webhook processing failed: X-Gitlab-Token validation failed", http.StatusBadRequest)
		case r.Header.Get("X-Vss-ActivityId") != "":
			headers = append(headers, "azure")
		default:
			http.Error(w, "Unknown webhook event", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	accepted, err := probeWebhook(context.Background(), srv.Client(), srv.URL+"/api/webhook", appSetWebhookProbes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(headers, ",") != "github,gitlab,azure" || len(accepted) != 2 || accepted[0].Provider != "GitHub" || accepted[1].Provider != "Azure DevOps" {
		t.Fatalf("headers = %v, accepted = %+v", headers, accepted)
	}
	w := webhookWarnings("the ApplicationSet controller", accepted, true)
	if len(w) != 2 || !strings.Contains(w[0], "the webhook of the ApplicationSet controller accepts events of GitHub without a signature: webhook.github.secret in argocd-secret is empty") ||
		!strings.Contains(w[0], "restart it after a change") {
		t.Fatalf("warnings = %q", w)
	}
	for _, m := range w {
		if strings.Contains(m, ";") {
			t.Errorf("the message has a semicolon: %q", m)
		}
	}

	// Nothing listens.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if _, err := probeWebhook(context.Background(), http.DefaultClient, "http://"+addr+"/api/webhook", appSetWebhookProbes); !errors.Is(err, errNotListening) {
		t.Fatalf("err = %v", err)
	}
}
