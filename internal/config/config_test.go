package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReader(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"A":        "x",
		"N":        "42",
		"B":        "true",
		"D":        "30s",
		"S_FILE":   secretFile,
		"BAD_INT":  "nope",
		"BAD_BOOL": "maybe",
	}
	r := Reader{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	if r.String("A", "") != "x" || r.String("MISSING", "def") != "def" {
		t.Error("String")
	}
	if r.Int("N", 0) != 42 || !r.Bool("B", false) || r.Duration("D", 0) != 30*time.Second {
		t.Error("typed values")
	}
	if r.Secret("S", true) != "s3cret" {
		t.Error("Secret from file")
	}
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	r.Int("BAD_INT", 0)
	r.Bool("BAD_BOOL", false)
	r.Required("NOT_SET")
	err := r.Err()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"BAD_INT", "BAD_BOOL", "NOT_SET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
