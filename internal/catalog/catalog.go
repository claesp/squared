// Package catalog loads and checks package definitions. A package is a
// versioned set of components with parameters. Platform administrators
// write packages. Tenants select a package and give parameter values.
package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Parameter types.
const (
	TypeString  = "string"
	TypeInteger = "integer"
	TypeBoolean = "boolean"
	TypeImage   = "image" // A container image in the registry, "name:tag" or "name@sha256:...".
	TypeEnv     = "env"   // Environment variables, one "NAME=value" for each line.
)

// Package is a package definition.
type Package struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Parameters  []Parameter `json:"parameters"`
	Components  []Component `json:"components"`
}

// Parameter is an input value of a package.
type Parameter struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Example     string   `json:"example,omitempty"` // Shown in empty form fields. Load uses it to check the package.
	Enum        []string `json:"enum,omitempty"`    // Only for "string".
	Pattern     string   `json:"pattern,omitempty"` // Only for "string". Anchored regular expression.
	Min         *int64   `json:"min,omitempty"`     // Only for "integer".
	Max         *int64   `json:"max,omitempty"`     // Only for "integer".
	Project     string   `json:"project,omitempty"` // Only for "image". Registry project if not the project of the tenant.

	pattern *regexp.Regexp
}

// Component is one part of a package, for example a web server or a
// scheduled job. Type selects the renderer. A string setting with the form
// "${name}" gets the value of the parameter "name".
type Component struct {
	Name     string                     `json:"name"`
	Type     string                     `json:"type"`
	Settings map[string]json.RawMessage `json:"settings"`
}

// Ref returns "name@version".
func (p *Package) Ref() string { return p.Name + "@" + p.Version }

// Parameter returns the parameter with a name, or nil.
func (p *Package) Parameter(name string) *Parameter {
	for i := range p.Parameters {
		if p.Parameters[i].Name == name {
			return &p.Parameters[i]
		}
	}
	return nil
}

// Checker checks the settings of a component of a type. It returns an
// error if the type is not known or the settings are not valid. The render
// package supplies the checker, to keep this package free of render logic.
type Checker func(componentType string, settings map[string]json.RawMessage) error

var (
	namePattern    = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	paramPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	refPattern     = regexp.MustCompile(`^\$\{([a-z][a-z0-9_]*)\}$`)
	// Component names have no '-'. Object names are "<deployment>-<component>",
	// thus the last '-' separates the two parts and names are unique.
	componentPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)

// Parse decodes and checks a package definition.
func Parse(data []byte, checker Checker) (*Package, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Package
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode package: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("decode package: unexpected data after the package")
	}
	if err := p.check(checker); err != nil {
		return nil, fmt.Errorf("package %s: %w", p.Ref(), err)
	}
	return &p, nil
}

func (p *Package) check(checker Checker) error {
	if !namePattern.MatchString(p.Name) || len(p.Name) > 40 {
		return fmt.Errorf("name %q is not valid", p.Name)
	}
	if !versionPattern.MatchString(p.Version) {
		return fmt.Errorf("version %q must have the form MAJOR.MINOR.PATCH", p.Version)
	}
	if p.Title == "" {
		return fmt.Errorf("title is missing")
	}
	seen := map[string]bool{}
	for i := range p.Parameters {
		prm := &p.Parameters[i]
		if !paramPattern.MatchString(prm.Name) || len(prm.Name) > 40 {
			return fmt.Errorf("parameter name %q is not valid", prm.Name)
		}
		if seen[prm.Name] {
			return fmt.Errorf("parameter %q is defined two times", prm.Name)
		}
		seen[prm.Name] = true
		if err := prm.check(); err != nil {
			return fmt.Errorf("parameter %q: %w", prm.Name, err)
		}
	}
	if len(p.Components) == 0 {
		return fmt.Errorf("package has no components")
	}
	used := map[string]bool{}
	for _, c := range p.Components {
		if !componentPattern.MatchString(c.Name) || len(c.Name) > 20 {
			return fmt.Errorf("component name %q must have 1 to 20 lowercase letters or digits", c.Name)
		}
		if used[c.Name] {
			return fmt.Errorf("component %q is defined two times", c.Name)
		}
		used[c.Name] = true
		for key, raw := range c.Settings {
			if name, ok := reference(raw); ok && !seen[name] {
				return fmt.Errorf("component %q: setting %q refers to unknown parameter %q", c.Name, key, name)
			}
		}
		// Images must come from the registry. Thus the image setting must
		// refer to an image parameter: the worker resolves those.
		imgParam, ok := reference(c.Settings["image"])
		if !ok || p.Parameter(imgParam).Type != TypeImage {
			return fmt.Errorf("component %q: setting \"image\" must refer to a parameter of type image", c.Name)
		}
		// Check the settings with the defaults of the parameters. This
		// finds unknown and badly typed settings at load time.
		settings, err := p.Settings(c, p.sampleValues())
		if err != nil {
			return fmt.Errorf("component %q: %w", c.Name, err)
		}
		if err := checker(c.Type, settings); err != nil {
			return fmt.Errorf("component %q: %w", c.Name, err)
		}
	}
	return nil
}

func (prm *Parameter) check() error {
	if prm.Title == "" {
		return fmt.Errorf("title is missing")
	}
	switch prm.Type {
	case TypeString, TypeInteger, TypeBoolean, TypeImage, TypeEnv:
	default:
		return fmt.Errorf("unknown type %q", prm.Type)
	}
	if len(prm.Enum) > 0 && prm.Type != TypeString {
		return fmt.Errorf("enum is only valid for type string")
	}
	if prm.Pattern != "" {
		if prm.Type != TypeString {
			return fmt.Errorf("pattern is only valid for type string")
		}
		re, err := regexp.Compile("^(?:" + prm.Pattern + ")$")
		if err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
		prm.pattern = re
	}
	if (prm.Min != nil || prm.Max != nil) && prm.Type != TypeInteger {
		return fmt.Errorf("min and max are only valid for type integer")
	}
	if prm.Min != nil && prm.Max != nil && *prm.Min > *prm.Max {
		return fmt.Errorf("min is larger than max")
	}
	if prm.Project != "" && prm.Type != TypeImage {
		return fmt.Errorf("project is only valid for type image")
	}
	if prm.Required && prm.Default != nil {
		return fmt.Errorf("a required parameter must not have a default")
	}
	if prm.Example != "" && prm.Type != TypeString && prm.Type != TypeImage {
		return fmt.Errorf("example is only valid for types string and image")
	}
	if prm.Example != "" {
		if _, err := prm.Normalize(prm.Example); err != nil {
			return fmt.Errorf("example: %w", err)
		}
	}
	if prm.Required && prm.Type == TypeString && prm.Example == "" {
		return fmt.Errorf("a required string parameter must have an example")
	}
	if prm.Default != nil {
		v, err := prm.Normalize(prm.Default)
		if err != nil {
			return fmt.Errorf("default: %w", err)
		}
		prm.Default = v
	}
	return nil
}

// sampleValues returns a complete set of values to check settings. It uses
// the defaults, or a typical value for parameters without a default.
func (p *Package) sampleValues() map[string]any {
	values := map[string]any{}
	for _, prm := range p.Parameters {
		if prm.Default != nil {
			values[prm.Name] = prm.Default
			continue
		}
		switch prm.Type {
		case TypeInteger:
			n := int64(1)
			if prm.Min != nil {
				n = *prm.Min
			}
			values[prm.Name] = n
		case TypeBoolean:
			values[prm.Name] = false
		case TypeImage:
			values[prm.Name] = "registry.invalid/project/image@sha256:" + strings.Repeat("0", 64)
		case TypeEnv:
			values[prm.Name] = map[string]string{}
		default:
			switch {
			case prm.Example != "":
				values[prm.Name] = prm.Example
			case len(prm.Enum) > 0:
				values[prm.Name] = prm.Enum[0]
			}
		}
	}
	return values
}

// reference returns the parameter name if raw is a string "${name}".
func reference(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	m := refPattern.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Settings returns the settings of a component with the references
// replaced by values. values must be complete, as Resolve returns.
func (p *Package) Settings(c Component, values map[string]any) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(c.Settings))
	for key, raw := range c.Settings {
		name, ok := reference(raw)
		if !ok {
			out[key] = raw
			continue
		}
		v, ok := values[name]
		if !ok || v == nil {
			continue // An optional parameter without value. The setting is not set.
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("setting %q: %w", key, err)
		}
		out[key] = b
	}
	return out, nil
}

// Catalog is a set of packages. A Catalog does not change after Load.
type Catalog struct {
	packages map[string][]*Package // Name to versions, the newest first.
}

// Load reads all "*.json" files in fsys. Each file contains one package.
func Load(fsys fs.FS, checker Checker) (*Catalog, error) {
	c := &Catalog{packages: map[string][]*Package{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(name) != ".json" {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		p, err := Parse(data, checker)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return c.add(p)
	})
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	for _, versions := range c.packages {
		sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i].Version, versions[j].Version) > 0 })
	}
	return c, nil
}

func (c *Catalog) add(p *Package) error {
	for _, q := range c.packages[p.Name] {
		if q.Version == p.Version {
			return fmt.Errorf("package %s is defined two times", p.Ref())
		}
	}
	c.packages[p.Name] = append(c.packages[p.Name], p)
	return nil
}

// Latest returns the newest version of each package, ordered by name.
func (c *Catalog) Latest() []*Package {
	list := make([]*Package, 0, len(c.packages))
	for _, versions := range c.packages {
		list = append(list, versions[0])
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Versions returns all versions of a package, the newest first.
func (c *Catalog) Versions(name string) []*Package { return c.packages[name] }

// Get returns a version of a package. If version is "", Get returns the
// newest version.
func (c *Catalog) Get(name, version string) (*Package, bool) {
	versions := c.packages[name]
	if len(versions) == 0 {
		return nil, false
	}
	if version == "" {
		return versions[0], true
	}
	for _, p := range versions {
		if p.Version == version {
			return p, true
		}
	}
	return nil, false
}

// compareVersions compares two valid versions "MAJOR.MINOR.PATCH".
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range 3 {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
