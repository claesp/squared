package catalog_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"squared/internal/catalog"
	"squared/internal/domain"
	"squared/internal/render"
)

func TestLoadProjectCatalog(t *testing.T) {
	c, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Latest()) < 4 {
		t.Fatalf("expected at least 4 packages, got %d", len(c.Latest()))
	}
	if _, ok := c.Get("web-app", ""); !ok {
		t.Fatal("web-app is missing")
	}
}

const validPackage = `{
  "name": "demo", "version": "1.2.3", "title": "Demo",
  "parameters": [
    {"name": "image", "title": "Image", "type": "image", "required": true},
    {"name": "replicas", "title": "Replicas", "type": "integer", "default": 2, "min": 1, "max": 5},
    {"name": "mode", "title": "Mode", "type": "string", "default": "a", "enum": ["a", "b"]},
    {"name": "debug", "title": "Debug", "type": "boolean", "default": false},
    {"name": "env", "title": "Env", "type": "env"}
  ],
  "components": [{"name": "web", "type": "web", "settings": {"image": "${image}", "replicas": "${replicas}", "env": "${env}"}}]
}`

func TestParseRejectsBadPackages(t *testing.T) {
	tests := map[string]string{
		"unknown field":     strings.Replace(validPackage, `"title": "Demo"`, `"title": "Demo", "x": 1`, 1),
		"bad version":       strings.Replace(validPackage, `1.2.3`, `1.2`, 1),
		"unknown reference": strings.Replace(validPackage, `"${replicas}"`, `"${nothing}"`, 1),
		"unknown type":      strings.Replace(validPackage, `"type": "web"`, `"type": "database"`, 1),
		"bad default":       strings.Replace(validPackage, `"default": 2`, `"default": 9`, 1),
		"literal image":     strings.Replace(validPackage, `"image": "${image}"`, `"image": "docker.io/nginx:1"`, 1),
		"bad setting":       strings.Replace(validPackage, `"env": "${env}"}`, `"env": "${env}", "schedule": "* * * * *"}`, 1),
		"required default":  strings.Replace(validPackage, `"required": true`, `"required": true, "default": "x:1"`, 1),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := catalog.Parse([]byte(data), render.Check); err == nil {
				t.Fatal("Parse accepted a bad package")
			}
		})
	}
}

func TestResolve(t *testing.T) {
	p, err := catalog.Parse([]byte(validPackage), render.Check)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Resolve(map[string]any{"image": "shop/web:1.0", "replicas": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if got["replicas"] != int64(3) || got["mode"] != "a" || got["debug"] != false || got["image"] != "shop/web:1.0" {
		t.Fatalf("unexpected values: %#v", got)
	}

	_, err = p.Resolve(map[string]any{"replicas": float64(9), "mode": "c", "other": 1})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	for _, field := range []string{"image", "replicas", "mode", "other"} {
		if verr.FieldMessage(field) == "" {
			t.Errorf("no error for field %s: %v", field, verr)
		}
	}
}

func TestParseForm(t *testing.T) {
	p, err := catalog.Parse([]byte(validPackage), render.Check)
	if err != nil {
		t.Fatal(err)
	}
	in, err := p.ParseForm(map[string]string{
		"image":    "web:2",
		"replicas": "4",
		"debug":    "on",
		"env":      "# comment\nA=1\n\nB = two=2\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	env := got["env"].(map[string]string)
	if got["replicas"] != int64(4) || got["debug"] != true || env["A"] != "1" || env["B"] != " two=2" {
		t.Fatalf("unexpected values: %#v", got)
	}
	if _, err := p.ParseForm(map[string]string{"env": "not a pair"}); err == nil {
		t.Fatal("bad env line accepted")
	}
}

func TestParseImage(t *testing.T) {
	good := []string{"web:1.0", "shop/web:v2", "a.b/c-d:latest_1", "web@sha256:" + strings.Repeat("a", 64)}
	for _, s := range good {
		if _, err := catalog.ParseImage(s); err != nil {
			t.Errorf("ParseImage(%q): %v", s, err)
		}
	}
	bad := []string{"web", "Web:1", "registry.example.com:5000/web", "web:", "web@sha256:abc", "../web:1", "web:1/2"}
	for _, s := range bad {
		if _, err := catalog.ParseImage(s); err == nil {
			t.Errorf("ParseImage(%q) accepted a bad reference", s)
		}
	}
}

func TestVersionsOrder(t *testing.T) {
	fs := map[string]string{
		"a.json": strings.Replace(validPackage, "1.2.3", "1.10.0", 1),
		"b.json": strings.Replace(validPackage, "1.2.3", "1.9.0", 1),
		"c.json": validPackage,
	}
	c, err := catalog.Load(mapFS(fs), render.Check)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Get("demo", "")
	if p.Version != "1.10.0" {
		t.Fatalf("latest version is %s, expected 1.10.0", p.Version)
	}
	if len(c.Versions("demo")) != 3 {
		t.Fatal("expected 3 versions")
	}
}
