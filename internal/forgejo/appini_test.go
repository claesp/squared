package forgejo

import (
	"encoding/json"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadINISection(t *testing.T) {
	data := []byte(`APP_NAME = Forgejo
[security]
SECRET_KEY = not-in-the-answer

[oauth2_client]
; a comment
ACCOUNT_LINKING = login
USERNAME: nickname
ENABLE_AUTO_REGISTRATION = "true"
UPDATE_AVATAR = false # inline comment
OPENID_CONNECT_SCOPES = ` + "`openid email`" + `
account_linking = auto
ACCOUNT_LINKING = disabled

[OAUTH2_CLIENT]
ACCOUNT_LINKING = auto
`)
	got := ReadINISection(data, "oauth2_client")
	want := map[string]string{
		"ACCOUNT_LINKING":          "disabled", // The last key wins.
		"USERNAME":                 "nickname",
		"ENABLE_AUTO_REGISTRATION": "true",
		"UPDATE_AVATAR":            "false",
		"OPENID_CONNECT_SCOPES":    "openid email",
		"account_linking":          "auto", // Another key for Forgejo: names are case-sensitive.
	}
	if !maps.Equal(got, want) {
		t.Fatalf("section = %v, want %v", got, want)
	}
}

func TestSectionReporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.ini")
	if err := os.WriteFile(path, []byte("[security]\nSECRET_KEY = s3cret\n[oauth2_client]\nACCOUNT_LINKING = disabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewSectionReporter(path, "oauth2_client")
	if err := os.WriteFile(path, []byte("[oauth2_client]\nACCOUNT_LINKING = auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth2-client", nil))
	var rep SectionReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Start["ACCOUNT_LINKING"] != "disabled" || rep.Now["ACCOUNT_LINKING"] != "auto" || rep.Error != "" {
		t.Fatalf("report = %+v", rep)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("the report has another section")
	}
}

func TestSectionReporterWithoutFile(t *testing.T) {
	r := NewSectionReporter(filepath.Join(t.TempDir(), "missing.ini"), "oauth2_client")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth2-client", nil))
	var rep SectionReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Start != nil || rep.Now != nil || !strings.Contains(rep.Error, "at the start") {
		t.Fatalf("report = %+v", rep)
	}
}
