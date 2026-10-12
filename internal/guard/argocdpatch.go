package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// patchParameter takes the place of a value from the parameters of the
// generators in a template patch.
const patchParameter = "squared-guard-parameter"

// templateAction is an action of a Go template, or a parameter of the
// simple templates of Argo CD.
var templateAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

// controlWords are the actions of a Go template that write no text
// themselves.
var controlWords = []string{"if", "else", "end", "range", "with", "break", "continue"}

// isControl tells if an action writes no text: a control action, a
// comment, or a variable.
func isControl(action string) bool {
	inner := strings.TrimSuffix(strings.TrimPrefix(action, "{{"), "}}")
	inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(inner, "-"), "-"))
	if strings.HasPrefix(inner, "/*") {
		return true
	}
	f := strings.Fields(inner)
	switch {
	case len(f) == 0:
		return false
	case slices.Contains(controlWords, f[0]):
		return true
	default:
		return strings.HasPrefix(f[0], "$") && len(f) > 1 && (f[1] == ":=" || f[1] == "=")
	}
}

// patchSkeleton returns the YAML of a template patch for each value of
// the parameters. It removes the lines with only control actions, thus
// it keeps the lines of all branches, and it puts patchParameter in the
// place of each other action. A line with only an action that writes
// text can write each key, thus it is an error.
func patchSkeleton(patch string) (string, error) {
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		actions := templateAction.FindAllString(line, -1)
		if strings.Contains(templateAction.ReplaceAllString(line, ""), "{{") {
			return "", errors.New("an action has more than one line")
		}
		if len(actions) == 0 {
			out = append(out, line)
			continue
		}
		if strings.TrimSpace(templateAction.ReplaceAllString(line, "")) == "" {
			if !slices.ContainsFunc(actions, func(a string) bool { return !isControl(a) }) {
				continue
			}
			return "", errors.New("a line writes text from the parameters, which can be each key")
		}
		out = append(out, templateAction.ReplaceAllStringFunc(line, func(a string) string {
			return patchParameter
		}))
	}
	return strings.Join(out, "\n"), nil
}

// templatedKey returns the first key that comes from the parameters, or "".
func templatedKey(v any) string {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if strings.Contains(k, patchParameter) {
				return strings.ReplaceAll(k, patchParameter, "{{...}}")
			}
			if t := templatedKey(v[k]); t != "" {
				return t
			}
		}
	case []any:
		for _, e := range v {
			if t := templatedKey(e); t != "" {
				return t
			}
		}
	}
	return ""
}

// pick returns the keys of an object that are in keys.
func pick(v any, keys ...string) map[string]any {
	m, _ := v.(map[string]any)
	out := map[string]any{}
	for _, k := range keys {
		if e, ok := m[k]; ok {
			out[k] = e
		}
	}
	return out
}

// checkTemplatePatch returns the problems of the template patch of an
// ApplicationSet. Argo CD applies it to each application after the
// template, also without goTemplate, thus it has the same checks as a
// template. names are the applications of Squared (see checkAppTemplate).
func checkTemplatePatch(patch string, prefixes []string, names map[string]bool) ([]string, error) {
	skeleton, err := patchSkeleton(patch)
	if err != nil {
		return nil, err
	}
	// Argo CD reads the patch with this package too. The skeleton has the
	// lines of all branches, thus a key can be in it more than once: the
	// strict mode refuses that, because else the last value hides the
	// others.
	raw, err := yaml.YAMLToJSONStrict([]byte(skeleton))
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var p []string
	if k := templatedKey(v); k != "" {
		p = append(p, fmt.Sprintf("the key %q comes from the parameters, thus it can be each field", k))
	}
	// Only these fields have checks. The values of other fields can have
	// another type after the parameters, for example prune.
	m, _ := v.(map[string]any)
	part, err := json.Marshal(map[string]any{
		"metadata": pick(m["metadata"], "name", "labels", "finalizers"),
		"spec":     pick(m["spec"], "project", "source", "sources", "sourceHydrator"),
	})
	if err != nil {
		return nil, err
	}
	tp, err := checkAppTemplate(part, prefixes, names)
	if err != nil {
		return nil, err
	}
	p = append(p, tp...)
	for i := range p {
		p[i] = "in its templatePatch, " + p[i]
	}
	return p, nil
}
