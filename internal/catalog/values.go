package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"squared/internal/domain"
)

const (
	maxStringLen = 1000
	maxEnvVars   = 50
	maxEnvValue  = 4096
)

var (
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repoPattern    = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagPattern     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Image is an image reference inside a registry project.
type Image struct {
	Repository string // For example "shop/web".
	Tag        string // Empty if Digest is set.
	Digest     string // For example "sha256:...".
}

// ParseImage parses "repository:tag" or "repository@sha256:...". The
// reference must not contain a registry host or a project: Squared adds
// them. A tag or digest is necessary, because an implicit "latest" tag is
// not reproducible.
func ParseImage(s string) (Image, error) {
	if repo, digest, ok := strings.Cut(s, "@"); ok {
		if !repoPattern.MatchString(repo) || len(repo) > 255 {
			return Image{}, fmt.Errorf("repository %q is not valid", repo)
		}
		if !digestPattern.MatchString(digest) {
			return Image{}, fmt.Errorf("digest must have the form sha256:<64 hex digits>")
		}
		return Image{Repository: repo, Digest: digest}, nil
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return Image{}, fmt.Errorf("must have a tag (name:tag) or a digest (name@sha256:...)")
	}
	repo, tag := s[:i], s[i+1:]
	if strings.Contains(tag, "/") {
		return Image{}, fmt.Errorf("must not contain a registry host")
	}
	if !repoPattern.MatchString(repo) || len(repo) > 255 {
		return Image{}, fmt.Errorf("repository %q is not valid", repo)
	}
	if !tagPattern.MatchString(tag) {
		return Image{}, fmt.Errorf("tag %q is not valid", tag)
	}
	return Image{Repository: repo, Tag: tag}, nil
}

// String returns the reference in the form that ParseImage accepts.
func (i Image) String() string {
	if i.Digest != "" {
		return i.Repository + "@" + i.Digest
	}
	return i.Repository + ":" + i.Tag
}

// Normalize checks a value for the parameter and returns it in its
// standard Go type: string, int64, bool or map[string]string.
func (prm *Parameter) Normalize(v any) (any, error) {
	switch prm.Type {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		if utf8.RuneCountInString(s) > maxStringLen {
			return nil, fmt.Errorf("must have at most %d characters", maxStringLen)
		}
		if len(prm.Enum) > 0 && !contains(prm.Enum, s) {
			return nil, fmt.Errorf("must be one of: %s", strings.Join(prm.Enum, ", "))
		}
		if prm.pattern != nil && !prm.pattern.MatchString(s) {
			return nil, fmt.Errorf("must match the pattern %s", prm.Pattern)
		}
		return s, nil
	case TypeInteger:
		n, err := toInt(v)
		if err != nil {
			return nil, err
		}
		if prm.Min != nil && n < *prm.Min {
			return nil, fmt.Errorf("must be at least %d", *prm.Min)
		}
		if prm.Max != nil && n > *prm.Max {
			return nil, fmt.Errorf("must be at most %d", *prm.Max)
		}
		return n, nil
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be true or false")
		}
		return b, nil
	case TypeImage:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		img, err := ParseImage(s)
		if err != nil {
			return nil, err
		}
		return img.String(), nil
	case TypeEnv:
		return toEnv(v)
	}
	return nil, fmt.Errorf("unknown type %q", prm.Type)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func toInt(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		if n != math.Trunc(n) || math.Abs(n) > 1<<53 {
			return 0, fmt.Errorf("must be a whole number")
		}
		return int64(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("must be a whole number")
		}
		return i, nil
	}
	return 0, fmt.Errorf("must be a whole number")
}

func toEnv(v any) (map[string]string, error) {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		for k, x := range m {
			out[k] = x
		}
	case map[string]any:
		for k, x := range m {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("value of %s must be a string", k)
			}
			out[k] = s
		}
	default:
		return nil, fmt.Errorf("must be an object with string values")
	}
	if len(out) > maxEnvVars {
		return nil, fmt.Errorf("must have at most %d variables", maxEnvVars)
	}
	for k, x := range out {
		if !envNamePattern.MatchString(k) || len(k) > 100 {
			return nil, fmt.Errorf("variable name %q is not valid", k)
		}
		if len(x) > maxEnvValue {
			return nil, fmt.Errorf("value of %s must have at most %d bytes", k, maxEnvValue)
		}
	}
	return out, nil
}

// Resolve checks the input values and adds the defaults. The result has a
// value for each parameter that has an input value or a default. Field
// names in the error are parameter names.
func (p *Package) Resolve(input map[string]any) (map[string]any, error) {
	var verr domain.ValidationError
	for name := range input {
		if p.Parameter(name) == nil {
			verr.Add(name, "is not a parameter of package "+p.Name)
		}
	}
	out := map[string]any{}
	for i := range p.Parameters {
		prm := &p.Parameters[i]
		v, ok := input[prm.Name]
		if s, isString := v.(string); isString && s == "" {
			ok = false // An empty string means "no value".
		}
		if !ok || v == nil {
			switch {
			case prm.Default != nil:
				out[prm.Name] = prm.Default
			case prm.Required:
				verr.Add(prm.Name, "is required")
			}
			continue
		}
		n, err := prm.Normalize(v)
		if err != nil {
			verr.Add(prm.Name, err.Error())
			continue
		}
		out[prm.Name] = n
	}
	if err := verr.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseForm changes HTML form values into input values for Resolve. A
// boolean parameter is true if the form has the value "on" or "true".
func (p *Package) ParseForm(form map[string]string) (map[string]any, error) {
	var verr domain.ValidationError
	out := map[string]any{}
	for _, prm := range p.Parameters {
		s := strings.TrimSpace(form[prm.Name])
		switch prm.Type {
		case TypeBoolean:
			out[prm.Name] = s == "on" || s == "true"
		case TypeInteger:
			if s == "" {
				continue
			}
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				verr.Add(prm.Name, "must be a whole number")
				continue
			}
			out[prm.Name] = n
		case TypeEnv:
			env, err := ParseEnvLines(form[prm.Name])
			if err != nil {
				verr.Add(prm.Name, err.Error())
				continue
			}
			out[prm.Name] = env
		default:
			if s == "" {
				continue // Resolve uses the default or reports a missing value.
			}
			out[prm.Name] = s
		}
	}
	if err := verr.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseEnvLines parses lines of "NAME=value". It ignores empty lines and
// lines that start with "#".
func ParseEnvLines(text string) (map[string]string, error) {
	env := map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !envNamePattern.MatchString(name) {
			return nil, fmt.Errorf("line %d must have the form NAME=value", i+1)
		}
		env[name] = value
	}
	return env, nil
}

// FormValue returns a value as text for an HTML form field.
func FormValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "on"
		}
		return ""
	case map[string]string:
		return envLines(x)
	case map[string]any:
		m, err := toEnv(x)
		if err != nil {
			return ""
		}
		return envLines(m)
	}
	return fmt.Sprint(v)
}

func envLines(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	return b.String()
}
