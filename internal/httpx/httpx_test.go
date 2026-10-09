package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"squared/internal/metrics"
	"squared/internal/store/storetest"
)

func TestLogCountsRoutes(t *testing.T) {
	reg := &metrics.Registry{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tenants/{tenant}", func(w http.ResponseWriter, r *http.Request) {})
	h := Log(storetest.Logger(), reg, LimitBody(10, mux))
	for _, p := range []string{"/tenants/a", "/tenants/b", "/unknown"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	var b strings.Builder
	reg.Write(&b)
	for _, want := range []string{
		`squared_http_requests_total{method="GET",route="GET /tenants/{tenant}",status="2xx"} 2`,
		`squared_http_requests_total{method="GET",route="none",status="4xx"} 1`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %s in\n%s", want, b.String())
		}
	}
}
