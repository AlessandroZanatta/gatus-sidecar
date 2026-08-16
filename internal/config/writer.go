package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadBase reads and parses the operator-maintained portion of the Gatus config:
// storage, alerting providers, security, ui, and anything else that is genuinely
// a singleton. An empty path yields an empty base, which is useful in tests and
// for a sidecar that owns the whole file.
//
// Any endpoints key in the base is dropped, since this sidecar owns that list
// and silently merging the two would make it impossible to tell where an
// endpoint came from.
func LoadBase(path string) (Object, error) {
	if path == "" {
		return Object{}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read base config: %w", err)
	}

	var base Object
	if err := yaml.Unmarshal(raw, &base); err != nil {
		return nil, fmt.Errorf("parse base config %s: %w", path, err)
	}
	if base == nil {
		base = Object{}
	}
	delete(base, endpointsKey)
	return base, nil
}

// Marshal serialises a configuration document.
//
// Indentation is set to 2 to match how Gatus configs are conventionally written,
// which keeps the generated file readable for anyone who execs into the pod to
// see what the sidecar actually produced.
func Marshal(doc Object) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return buf.Bytes(), nil
}

// Writer publishes the rendered configuration to a file Gatus watches.
type Writer struct {
	path string

	// last is the content last successfully written, so an unchanged render can
	// skip the write entirely. Gatus reloads whenever the file changes, and
	// reloading for a no-op reconcile would restart every check's interval.
	last []byte
}

// NewWriter returns a Writer targeting path.
func NewWriter(path string) *Writer { return &Writer{path: path} }

// Path returns the destination file.
func (w *Writer) Path() string { return w.path }

// Write publishes content, reporting whether anything actually changed.
//
// The write is atomic: content goes to a temporary file in the same directory
// and is renamed into place, so Gatus can never read a half-written config.
// A failed write leaves the previous file intact rather than truncating it,
// because a stale configuration still monitors things and an empty one does not.
func (w *Writer) Write(content []byte) (changed bool, err error) {
	if w.last != nil && bytes.Equal(w.last, content) {
		return false, nil
	}

	// On the first write after a restart, compare against what is already on
	// disk so a restarted sidecar does not needlessly reload Gatus.
	if w.last == nil {
		if existing, readErr := os.ReadFile(w.path); readErr == nil && bytes.Equal(existing, content) {
			w.last = content
			return false, nil
		}
	}

	dir := filepath.Dir(w.path)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create output directory %s: %w", dir, err)
	}

	// The temporary file must share a directory with the target: rename is only
	// atomic within a filesystem, and /tmp is frequently a different one.
	tmp, err := os.CreateTemp(dir, ".gatus-config-*.yaml")
	if err != nil {
		return false, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(content); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write temp file: %w", err)
	}
	// Flush before the rename so a crash cannot leave the target pointing at an
	// empty inode.
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return false, fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Chmod(tmpName, 0o644); err != nil {
		return false, fmt.Errorf("chmod temp file: %w", err)
	}
	if err = os.Rename(tmpName, w.path); err != nil {
		return false, fmt.Errorf("rename into %s: %w", w.path, err)
	}

	w.last = content
	return true, nil
}
