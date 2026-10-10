package forgejo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// ReadINISection returns the keys and values of one section of a Forgejo
// app.ini file. It reads the file as Forgejo 15 does (go-ini with the
// options of Forgejo): "=" or ":" separates a key and its value, "#" and
// ";" start a comment, quotes around a value are removed, and a later key
// with the same name replaces an earlier one. Section and key names are
// case-sensitive, as in Forgejo.
func ReadINISection(data []byte, section string) map[string]string {
	out := map[string]string{}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if end := strings.IndexByte(line, ']'); end > 0 {
				current = strings.TrimSpace(line[1:end])
			}
			continue
		}
		if current != section {
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i < 0 {
			out[line] = "" // A key without a value.
			continue
		}
		out[strings.TrimSpace(line[:i])] = iniValue(strings.TrimSpace(line[i+1:]))
	}
	return out
}

// iniValue removes quotes or an inline comment from a value.
func iniValue(v string) string {
	for _, q := range []string{`"""`, "`", `"`, "'"} {
		if strings.HasPrefix(v, q) {
			if end := strings.Index(v[len(q):], q); end >= 0 {
				return v[len(q) : len(q)+end]
			}
		}
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// HiddenValue is the value of a key that the reporter does not show.
const HiddenValue = "<hidden>"

// reportedSection is a section of app.ini that the reporter serves.
type reportedSection struct {
	name string
	// keys are the only keys that the reporter serves. nil: all keys.
	keys []string
	// values are the keys whose values the reporter serves; the other
	// keys get HiddenValue. nil: all values.
	values []string
}

// signInSections are the sections with the sign-in and registration
// settings. [service] has secrets (for example RECAPTCHA_SECRET), thus the
// reporter serves its key names, but only the values of the sign-in and
// registration settings. Of [security], it serves only INSTALL_LOCK, which
// changes the default of ENABLE_OPENID_SIGNIN. [admin] has the features
// that users cannot use, for example "manage_password".
var signInSections = []reportedSection{
	{name: "admin"},
	{name: "oauth2_client"},
	{name: "openid"},
	{name: "service", values: []string{
		"ALLOW_ONLY_EXTERNAL_REGISTRATION", "ALLOW_ONLY_INTERNAL_REGISTRATION", "DISABLE_REGISTRATION",
		"ENABLE_BASIC_AUTHENTICATION", "ENABLE_INTERNAL_SIGNIN",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION", "ENABLE_REVERSE_PROXY_AUTHENTICATION_API",
		"ENABLE_REVERSE_PROXY_AUTO_REGISTRATION", "ENABLE_REVERSE_PROXY_EMAIL", "ENABLE_REVERSE_PROXY_FULL_NAME",
		"REGISTER_EMAIL_CONFIRM", "REGISTER_MANUAL_CONFIRM", "REQUIRE_EXTERNAL_REGISTRATION_PASSWORD",
		"SHOW_REGISTRATION_BUTTON", "DEFAULT_USER_IS_RESTRICTED",
	}},
	{name: "security", keys: []string{"INSTALL_LOCK"}},
}

// readSignInSections returns the served part of the sign-in sections.
func readSignInSections(data []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, s := range signInSections {
		section := ReadINISection(data, s.name)
		served := map[string]string{}
		for k, v := range section {
			if s.keys != nil && !slices.Contains(s.keys, k) {
				continue
			}
			if s.values != nil && !slices.Contains(s.values, k) {
				v = HiddenValue
			}
			served[k] = v
		}
		out[s.name] = served
	}
	return out
}

// ConfigReport is the answer of a ConfigReporter: the served part of the
// sign-in sections, by section and key.
type ConfigReport struct {
	// Start are the values when the reporter started. The reporter runs in
	// the pod of Forgejo and starts with Forgejo, after the init containers
	// wrote app.ini. Forgejo reads app.ini only at its start, thus Start
	// has the values that Forgejo uses.
	Start     map[string]map[string]string `json:"start"`
	StartedAt time.Time                    `json:"startedAt"`
	// Now are the values in the file now. They apply after the next start
	// of Forgejo.
	Now   map[string]map[string]string `json:"now"`
	Error string                       `json:"error,omitempty"`
}

// ConfigReporter serves the sign-in sections of app.ini as JSON (see
// signInSections). It never serves secrets.
type ConfigReporter struct {
	path      string
	start     map[string]map[string]string
	startErr  error
	startedAt time.Time
}

// NewConfigReporter reads the sections one time, for the start values. A
// read error goes into the reports, and the reporter keeps running: it is
// a sidecar of Forgejo, and a container that stops makes the pod of
// Forgejo not ready.
func NewConfigReporter(path string) *ConfigReporter {
	r := &ConfigReporter{path: path, startedAt: time.Now().UTC()}
	data, err := os.ReadFile(path)
	if err != nil {
		r.startErr = err
	} else {
		r.start = readSignInSections(data)
	}
	return r
}

func (r *ConfigReporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	rep := ConfigReport{Start: r.start, StartedAt: r.startedAt}
	var errs []string
	if r.startErr != nil {
		errs = append(errs, fmt.Sprintf("read app.ini at the start: %v", r.startErr))
	}
	if data, err := os.ReadFile(r.path); err != nil {
		errs = append(errs, fmt.Sprintf("read app.ini: %v", err))
	} else {
		rep.Now = readSignInSections(data)
	}
	rep.Error = strings.Join(errs, "; ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(rep)
}
