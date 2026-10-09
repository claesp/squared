package render_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"squared/internal/catalog"
	"squared/internal/render"
)

var update = flag.Bool("update", false, "write the golden files")

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func loadCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestGolden renders each package of the project catalog and compares the
// result with the files in testdata. Run "go test ./internal/render
// -update" to write new golden files after an intended change.
func TestGolden(t *testing.T) {
	c := loadCatalog(t)
	cases := []struct {
		name   string
		pkg    string
		values map[string]any
	}{
		{"web-app-defaults", "web-app", map[string]any{"image": "shop:1.0.0"}},
		{"web-app-public", "web-app", map[string]any{
			"image": "shop:1.0.0", "replicas": float64(3), "host": "shop.example.com",
			"env": map[string]any{"LOG_LEVEL": "debug", "A_FIRST": "1"},
		}},
		{"worker", "worker", map[string]any{"image": "consumer:2.0.0"}},
		{"scheduled-job", "scheduled-job", map[string]any{"image": "report:1.0.0", "schedule": "0 3 * * *"}},
		{"web-with-worker", "web-with-worker", map[string]any{"image": "shop:1.0.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkg, ok := c.Get(tc.pkg, "")
			if !ok {
				t.Fatalf("package %s is missing", tc.pkg)
			}
			values, err := pkg.Resolve(tc.values)
			if err != nil {
				t.Fatal(err)
			}
			files, err := render.Render(render.Input{
				Tenant:     "acme",
				Namespace:  "acme-dev",
				Deployment: "shop",
				Package:    pkg,
				Values:     values,
				Images:     map[string]string{"image": "registry.test/acme/app@" + digest},
				IngressTLS: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			for _, f := range files {
				got.WriteString("# " + f.Path + "\n")
				got.Write(f.Content)
			}
			golden := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("output differs from %s; run with -update if the change is intended\n%s", golden, got.String())
			}
		})
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	c := loadCatalog(t)
	pkg, _ := c.Get("web-app", "")
	values, err := pkg.Resolve(map[string]any{"image": "a:1", "env": map[string]any{"Z": "1", "A": "2", "M": "3"}})
	if err != nil {
		t.Fatal(err)
	}
	in := render.Input{Tenant: "t", Namespace: "t-dev", Deployment: "d", Package: pkg, Values: values,
		Images: map[string]string{"image": "r/t/a@" + digest}}
	first, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := render.Render(in)
		if err != nil {
			t.Fatal(err)
		}
		for i := range first {
			if !bytes.Equal(first[i].Content, again[i].Content) {
				t.Fatal("two renders of the same input are different")
			}
		}
	}
}

func TestRenderRequiresDigest(t *testing.T) {
	c := loadCatalog(t)
	pkg, _ := c.Get("web-app", "")
	values, err := pkg.Resolve(map[string]any{"image": "a:1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = render.Render(render.Input{Tenant: "t", Namespace: "t-dev", Deployment: "d", Package: pkg, Values: values,
		Images: map[string]string{"image": "r/t/a:1"}})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expected a digest error, got %v", err)
	}
}

func TestRenderSecurityDefaults(t *testing.T) {
	c := loadCatalog(t)
	for _, pkg := range c.Latest() {
		sample := map[string]any{}
		for _, prm := range pkg.Parameters {
			if prm.Required {
				switch prm.Type {
				case catalog.TypeImage:
					sample[prm.Name] = "x:1"
				default:
					sample[prm.Name] = "0 1 * * *"
				}
			}
		}
		values, err := pkg.Resolve(sample)
		if err != nil {
			t.Fatalf("%s: %v", pkg.Name, err)
		}
		files, err := render.Render(render.Input{Tenant: "t", Namespace: "t-dev", Deployment: "d", Package: pkg,
			Values: values, Images: map[string]string{"image": "r/t/x@" + digest}})
		if err != nil {
			t.Fatalf("%s: %v", pkg.Name, err)
		}
		for _, f := range files {
			s := string(f.Content)
			if !strings.Contains(f.Path, "deployment-") && !strings.Contains(f.Path, "cronjob-") {
				continue
			}
			for _, want := range []string{
				`"allowPrivilegeEscalation": false`,
				`"readOnlyRootFilesystem": true`,
				`"runAsNonRoot": true`,
				`"automountServiceAccountToken": false`,
				`"RuntimeDefault"`,
				`"ALL"`,
			} {
				if !strings.Contains(s, want) {
					t.Errorf("%s %s: missing %s", pkg.Name, f.Path, want)
				}
			}
		}
	}
}
