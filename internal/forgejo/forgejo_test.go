package forgejo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBlobSHA(t *testing.T) {
	// Value from "printf 'hello\n' | git hash-object --stdin".
	if got := BlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("BlobSHA = %s", got)
	}
}

func TestDiff(t *testing.T) {
	current := map[string]string{
		"a/keep.json":   BlobSHA([]byte("same")),
		"a/change.json": BlobSHA([]byte("old")),
		"a/remove.json": "1234",
		"b/other.json":  "5678", // Not in scope.
	}
	desired := map[string][]byte{
		"a/keep.json":   []byte("same"),
		"a/change.json": []byte("new"),
		"a/add.json":    []byte("added"),
	}
	got := Diff(current, Prefix("a"), desired)
	want := []struct{ op, path string }{
		{"create", "a/add.json"},
		{"update", "a/change.json"},
		{"delete", "a/remove.json"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Operation != w.op || got[i].Path != w.path {
			t.Errorf("change %d = %s %s, want %s %s", i, got[i].Operation, got[i].Path, w.op, w.path)
		}
	}
	if got[1].SHA != current["a/change.json"] {
		t.Error("update does not have the old SHA")
	}
}

// TestSync checks the requests against a fake Forgejo server.
func TestSync(t *testing.T) {
	var committed map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/org/repo/branches/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"commit":{"id":"head1"}}`))
	})
	mux.HandleFunc("GET /api/v1/repos/org/repo/git/trees/head1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tree":[{"path":"d/x.json","type":"blob","sha":"` + BlobSHA([]byte("x")) + `"}],"truncated":false}`))
	})
	mux.HandleFunc("POST /api/v1/repos/org/repo/contents", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&committed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"commit":{"sha":"new1"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "secret", srv.Client())

	// No change: the result is the current head.
	sha, changed, err := c.Sync(context.Background(), "org", "repo", "main", Prefix("d"), map[string][]byte{"d/x.json": []byte("x")}, "msg")
	if err != nil || changed || sha != "head1" {
		t.Fatalf("Sync without change = %s, %v, %v", sha, changed, err)
	}

	// A change makes one commit.
	sha, changed, err = c.Sync(context.Background(), "org", "repo", "main", Prefix("d"), map[string][]byte{"d/y.json": []byte("y")}, "msg")
	if err != nil || !changed || sha != "new1" {
		t.Fatalf("Sync with change = %s, %v, %v", sha, changed, err)
	}
	files := committed["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("expected 2 file operations, got %v", files)
	}
	first := files[0].(map[string]any)
	if first["operation"] != "delete" || first["path"] != "d/x.json" {
		t.Errorf("unexpected first operation %v", first)
	}
	second := files[1].(map[string]any)
	content, _ := base64.StdEncoding.DecodeString(second["content"].(string))
	if second["operation"] != "create" || string(content) != "y" {
		t.Errorf("unexpected second operation %v", second)
	}
}

func TestUserNameFor(t *testing.T) {
	tests := map[string]string{
		"dev@example.com":          "dev",
		"Ada.Lovelace@example.com": "ada.lovelace",
		"a+tag@example.com":        "a-tag",
		"-x-@example.com":          "x",
		"@example.com":             "user",
		"averyveryveryverylongnameforsomebody@example.com": "averyveryveryverylongnameforso",
	}
	for email, want := range tests {
		if got := UserNameFor(email); got != want {
			t.Errorf("UserNameFor(%q) = %q, want %q", email, got, want)
		}
	}
}
