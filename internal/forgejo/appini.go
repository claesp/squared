package forgejo

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
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

// SectionReport is the answer of a SectionReporter.
type SectionReport struct {
	Section string `json:"section"`
	// Start are the keys and values when the reporter started. The
	// reporter runs in the pod of Forgejo and starts with Forgejo, after
	// the init containers wrote app.ini. Forgejo reads app.ini only at its
	// start, thus Start has the values that Forgejo uses.
	Start     map[string]string `json:"start"`
	StartedAt time.Time         `json:"startedAt"`
	// Now are the keys and values in the file now. They apply after the
	// next start of Forgejo.
	Now   map[string]string `json:"now"`
	Error string            `json:"error,omitempty"`
}

// SectionReporter serves one section of app.ini as JSON. It never serves
// other sections, which can have secrets.
type SectionReporter struct {
	path, section string
	start         map[string]string
	startErr      error
	startedAt     time.Time
}

// NewSectionReporter reads the section one time, for the start values. A
// read error goes into the reports, and the reporter keeps running: it is
// a sidecar of Forgejo, and a container that stops makes the pod of
// Forgejo not ready.
func NewSectionReporter(path, section string) *SectionReporter {
	r := &SectionReporter{path: path, section: section, startedAt: time.Now().UTC()}
	data, err := os.ReadFile(path)
	if err != nil {
		r.startErr = err
	} else {
		r.start = ReadINISection(data, section)
	}
	return r
}

func (r *SectionReporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	rep := SectionReport{Section: r.section, Start: maps.Clone(r.start), StartedAt: r.startedAt}
	var errs []string
	if r.startErr != nil {
		errs = append(errs, fmt.Sprintf("read app.ini at the start: %v", r.startErr))
	}
	if data, err := os.ReadFile(r.path); err != nil {
		errs = append(errs, fmt.Sprintf("read app.ini: %v", err))
	} else {
		rep.Now = ReadINISection(data, r.section)
	}
	rep.Error = strings.Join(errs, "; ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(rep)
}
