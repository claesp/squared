package guard

import (
	"encoding/json"
	"regexp"
	"strings"
)

// argoRBACGroups returns the subjects of the "g" lines of an Argo CD RBAC
// policy (CSV). A "g" line assigns a role to a subject:
//
//	g, /squared-admins, role:admin
//
// With "scopes: [groups]", a subject that starts with "/" is a group path.
// Other subjects are user names or roles; they are returned separately so
// that the guard can warn about short group names, which never match a
// full path in the groups claim.
func argoRBACGroups(csv string) (groups, others []string) {
	for _, line := range strings.Split(csv, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 || strings.TrimSpace(fields[0]) != "g" {
			continue
		}
		subject := strings.Trim(strings.TrimSpace(fields[1]), `"`)
		switch {
		case subject == "":
		case strings.HasPrefix(subject, "/"):
			groups = append(groups, subject)
		case !strings.HasPrefix(subject, "role:"):
			others = append(others, subject)
		}
	}
	return groups, others
}

var (
	forgejoGroupFlag = regexp.MustCompile(`--(admin-group|restricted-group)\s+"([^"]*)"`)
	forgejoTeamMap   = regexp.MustCompile(`--group-team-map\s+"((?:[^"\\]|\\.)*)"`)
)

// forgejoInitGroups returns the groups in the "auth add-oauth" and
// "auth update-oauth" commands of the init script of the Forgejo Helm
// chart: the administrator group, the restricted group and the keys of the
// group to team map. Forgejo has no API for its authentication sources;
// the chart applies this script at each start.
func forgejoInitGroups(script string) []string {
	var groups []string
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "auth add-oauth") && !strings.Contains(line, "auth update-oauth") {
			continue
		}
		for _, m := range forgejoGroupFlag.FindAllStringSubmatch(line, -1) {
			if m[2] != "" {
				groups = append(groups, m[2])
			}
		}
		for _, m := range forgejoTeamMap.FindAllStringSubmatch(line, -1) {
			groups = append(groups, teamMapGroups(strings.ReplaceAll(m[1], `\"`, `"`))...)
		}
	}
	return groups
}

// splitGroups splits a value with one or more group paths, separated by
// commas or white space.
func splitGroups(v string) []string {
	var out []string
	for _, g := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// teamMapGroups returns the groups of a Forgejo group to team map, which
// is JSON: {"/group": {"org": ["team"]}}. A value that is not valid JSON
// gives no groups.
func teamMapGroups(raw string) []string {
	var mapping map[string]any
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &mapping) != nil {
		return nil
	}
	groups := make([]string, 0, len(mapping))
	for g := range mapping {
		groups = append(groups, g)
	}
	return groups
}
