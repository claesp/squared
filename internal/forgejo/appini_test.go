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

func TestConfigReporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.ini")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[security]
INSTALL_LOCK = true
SECRET_KEY = s3cret-key
INTERNAL_TOKEN = s3cret-token
[service]
DISABLE_REGISTRATION = false
ALLOW_ONLY_EXTERNAL_REGISTRATION = true
RECAPTCHA_SECRET = s3cret-captcha
[oauth2_client]
ACCOUNT_LINKING = disabled
[openid]
ENABLE_OPENID_SIGNIN = false
[admin]
EXTERNAL_USER_DISABLE_FEATURES = manage_password
[database]
PASSWD = s3cret-db
`)
	r := NewConfigReporter(path)
	write("[oauth2_client]\nACCOUNT_LINKING = auto\n")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/sign-in", nil))
	var rep ConfigReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"oauth2_client": {"ACCOUNT_LINKING": "disabled"},
		"openid":        {"ENABLE_OPENID_SIGNIN": "false"},
		"service": {"DISABLE_REGISTRATION": "false", "ALLOW_ONLY_EXTERNAL_REGISTRATION": "true",
			"RECAPTCHA_SECRET": HiddenValue}, // The name, not the value.
		"security": {"INSTALL_LOCK": "true"}, // Only this key.
		"admin":    {"EXTERNAL_USER_DISABLE_FEATURES": "manage_password"},
	}
	for section, values := range want {
		if !maps.Equal(rep.Start[section], values) {
			t.Errorf("start [%s] = %v, want %v", section, rep.Start[section], values)
		}
	}
	if _, ok := rep.Start["database"]; ok {
		t.Error("the report has the section [database]")
	}
	if rep.Now["oauth2_client"]["ACCOUNT_LINKING"] != "auto" || len(rep.Now["service"]) != 0 || rep.Error != "" {
		t.Fatalf("now = %v, error %q", rep.Now, rep.Error)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("the report has a secret: %s", rec.Body.String())
	}
}

func TestConfigReporterWithoutFile(t *testing.T) {
	r := NewConfigReporter(filepath.Join(t.TempDir(), "missing.ini"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/sign-in", nil))
	var rep ConfigReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Start != nil || rep.Now != nil || !strings.Contains(rep.Error, "at the start") {
		t.Fatalf("report = %+v", rep)
	}
}
