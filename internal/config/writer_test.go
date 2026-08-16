package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base.yaml")

	content := `
security:
  oidc:
    client-secret: "${OIDC_CLIENT_SECRET}"
ui:
  default-sort-by: group
endpoints:
  - name: Stale
    url: http://stale
announcements: []
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	base, err := LoadBase(path)
	if err != nil {
		t.Fatalf("LoadBase: %v", err)
	}
	if base["ui"] == nil || base["security"] == nil {
		t.Errorf("base = %#v, want ui and security preserved", base)
	}
	// The sidecar owns the endpoints list; a stale one in the base would make
	// it impossible to tell where an endpoint came from.
	if _, present := base["endpoints"]; present {
		t.Errorf("base kept endpoints: %#v", base["endpoints"])
	}
	if base["announcements"] == nil {
		t.Errorf("base = %#v, want announcements preserved", base)
	}
}

func TestLoadBaseEdgeCases(t *testing.T) {
	dir := t.TempDir()

	t.Run("empty path yields an empty base", func(t *testing.T) {
		base, err := LoadBase("")
		if err != nil {
			t.Fatalf("LoadBase: %v", err)
		}
		if len(base) != 0 {
			t.Errorf("base = %#v, want empty", base)
		}
	})

	t.Run("empty file yields an empty base", func(t *testing.T) {
		path := filepath.Join(dir, "empty.yaml")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		base, err := LoadBase(path)
		if err != nil {
			t.Fatalf("LoadBase: %v", err)
		}
		if base == nil || len(base) != 0 {
			t.Errorf("base = %#v, want an empty non-nil object", base)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := LoadBase(filepath.Join(dir, "nope.yaml")); err == nil {
			t.Fatal("LoadBase() = nil error, want a failure")
		}
	})

	t.Run("malformed yaml is an error", func(t *testing.T) {
		path := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(path, []byte("key: [unclosed\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := LoadBase(path); err == nil {
			t.Fatal("LoadBase() = nil error, want a parse failure")
		}
	})
}

func TestWriterWritesAtomicallyAndSkipsNoOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	w := NewWriter(path)

	changed, err := w.Write([]byte("first\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !changed {
		t.Error("changed = false on the first write, want true")
	}
	if got := readFile(t, path); got != "first\n" {
		t.Errorf("file = %q, want %q", got, "first\n")
	}

	// Identical content must not touch the file: Gatus reloads on change, and a
	// reload restarts every check's interval for nothing.
	changed, err = w.Write([]byte("first\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if changed {
		t.Error("changed = true for identical content, want false")
	}

	changed, err = w.Write([]byte("second\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !changed {
		t.Error("changed = false for new content, want true")
	}
	if got := readFile(t, path); got != "second\n" {
		t.Errorf("file = %q, want %q", got, "second\n")
	}
}

func TestWriterSkipsWhenDiskAlreadyMatches(t *testing.T) {
	// A restarted sidecar re-renders the same config; it must not reload Gatus.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	changed, err := NewWriter(path).Write([]byte("same\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if changed {
		t.Error("changed = true, want false when the file already matches")
	}
}

func TestWriterLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	w := NewWriter(path)

	for _, content := range []string{"a\n", "b\n", "c\n"} {
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gatus-config-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the config file", len(entries))
	}
}

func TestWriterFailureLeavesPreviousFileIntact(t *testing.T) {
	// A stale config still monitors things; a truncated one does not.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	w := NewWriter(path)

	if _, err := w.Write([]byte("good\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Make the directory read-only so the temp file cannot be created.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := w.Write([]byte("bad\n")); err == nil {
		t.Skip("running as a user that bypasses directory permissions")
	}
	if got := readFile(t, path); got != "good\n" {
		t.Errorf("file = %q, want the previous content intact", got)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	doc := mustYAML(t, `
storage:
  type: postgres
endpoints:
  - name: X
    conditions: ["[STATUS] == 200"]
`)
	out, err := Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	back := mustYAML(t, string(out))
	if back["storage"] == nil || back["endpoints"] == nil {
		t.Errorf("round trip lost keys: %#v", back)
	}
	// Two-space indentation keeps the generated file readable in the pod.
	if !strings.Contains(string(out), "\n  type: postgres") {
		t.Errorf("output is not 2-space indented:\n%s", out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
