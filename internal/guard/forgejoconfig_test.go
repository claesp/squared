package guard

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"squared/internal/forgejo"
)

func safeOAuth2Client() map[string]string {
	return map[string]string{"ACCOUNT_LINKING": "disabled", "USERNAME": "nickname", "ENABLE_AUTO_REGISTRATION": "true", "UPDATE_AVATAR": "false"}
}

func TestCheckForgejoOAuth2Client(t *testing.T) {
	ok := forgejo.SectionReport{Section: "oauth2_client", Start: safeOAuth2Client(), Now: safeOAuth2Client()}
	if warnings, err := checkForgejoOAuth2Client(ok, nil); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	tests := []struct {
		name   string
		change func(r *forgejo.SectionReport)
		errMsg string
	}{
		{"auto", func(r *forgejo.SectionReport) { r.Start["ACCOUNT_LINKING"], r.Now["ACCOUNT_LINKING"] = "auto", "auto" },
			"ACCOUNT_LINKING is auto now"},
		{"auto after the next start", func(r *forgejo.SectionReport) { r.Now["ACCOUNT_LINKING"] = "auto" },
			"ACCOUNT_LINKING is auto after the next start"},
		{"changed file", func(r *forgejo.SectionReport) { r.Now["USERNAME"] = "email" }, "changed after the start of Forgejo"},
		{"lower case key", func(r *forgejo.SectionReport) { r.Start["account_linking"], r.Now["account_linking"] = "auto", "auto" },
			"unknown key account_linking"},
		{"renamed key", func(r *forgejo.SectionReport) {
			delete(r.Start, "ACCOUNT_LINKING")
			delete(r.Now, "ACCOUNT_LINKING")
			r.Start["ACCOUNT_LINKING_MODE"], r.Now["ACCOUNT_LINKING_MODE"] = "disabled", "disabled"
		}, "unknown key ACCOUNT_LINKING_MODE"},
		{"unreadable file", func(r *forgejo.SectionReport) { r.Now, r.Error = nil, "permission denied" }, "cannot read app.ini"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := forgejo.SectionReport{Section: "oauth2_client", Start: safeOAuth2Client(), Now: safeOAuth2Client()}
			tt.change(&r)
			if _, err := checkForgejoOAuth2Client(r, nil); err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}

	// Forgejo uses "login" without the key or with a value that is not
	// valid: a warning, not an error.
	for _, v := range []string{"", "login", "Auto"} {
		r := forgejo.SectionReport{Section: "oauth2_client", Start: safeOAuth2Client()}
		r.Start["ACCOUNT_LINKING"] = v
		r.Now = maps.Clone(r.Start)
		warnings, err := checkForgejoOAuth2Client(r, nil)
		if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "ACCOUNT_LINKING is login") {
			t.Fatalf("value %q: err %v, warnings %v", v, err, warnings)
		}
	}
}

func TestForgejoConfigSource(t *testing.T) {
	report := forgejo.SectionReport{Section: "oauth2_client", Start: safeOAuth2Client(), Now: safeOAuth2Client()}
	report.Now["ACCOUNT_LINKING"] = "auto"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}))
	defer srv.Close()
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{
		{Type: "forgejo-config", Service: "Forgejo", URL: srv.URL + "/oauth2-client"}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, _, err := sources[0].References(context.Background())
	if len(refs) != 0 || err == nil || !strings.Contains(err.Error(), "auto after the next start") {
		t.Fatalf("refs %v, err %v", refs, err)
	}
}
