package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"syscall"
)

// manifestVersion is the manifest.json format this CLI reads and writes.
const manifestVersion = 1

// Manifest lists every path the CLI created in the stack, recorded when it
// was created. It is the source of truth for what destroy may remove (§9.8).
type Manifest struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Entry is one path the CLI created.
type Entry struct {
	// Path is slash-separated and relative to the stack directory; "." is
	// the stack directory itself, when init created it.
	Path string `json:"path"`
	// Type is EntryFile or EntryDir: what the CLI created there.
	Type string `json:"type"`
}

// Entry types.
const (
	EntryFile = "file"
	EntryDir  = "dir"
)

// Has reports whether the manifest lists path.
func (m Manifest) Has(path string) bool {
	return slices.ContainsFunc(m.Entries, func(e Entry) bool { return e.Path == path })
}

// Manifest reads manifest.json. A stack without one has an empty manifest.
func (s *Stack) Manifest() (Manifest, error) {
	data, err := s.root.ReadFile(ManifestFile)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{Version: manifestVersion}, nil
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("reading %s: %w", s.Path(ManifestFile), err)
	}
	if m.Version != manifestVersion {
		return Manifest{}, fmt.Errorf("%s is version %d; this pic-sure understands version %d", s.Path(ManifestFile), m.Version, manifestVersion)
	}
	return m, nil
}

// record adds entries to manifest.json, along with manifest.json itself.
func (s *Stack) record(entries ...Entry) error {
	return s.updateManifest(func(m *Manifest) {
		for _, e := range slices.Concat(entries, []Entry{{Path: ManifestFile, Type: EntryFile}}) {
			if !m.Has(e.Path) {
				m.Entries = append(m.Entries, e)
			}
		}
	})
}

// forget drops path from manifest.json.
func (s *Stack) forget(path string) error {
	return s.updateManifest(func(m *Manifest) {
		m.Entries = slices.DeleteFunc(m.Entries, func(e Entry) bool { return e.Path == path })
	})
}

// updateManifest rewrites manifest.json with change applied, if that changes
// it. It re-reads the file under an flock on .pic-sure/, because commands
// that don't take the stack lock still create files (a read-only command's
// debug log), and must not drop another command's entries.
func (s *Stack) updateManifest(change func(*Manifest)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, err := s.root.Open(CLIDir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // closing releases the flock
	if err := flock(d, syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking %s: %w", s.Path(CLIDir), err)
	}

	m, err := s.Manifest()
	if err != nil {
		return err
	}
	before := slices.Clone(m.Entries)
	change(&m)
	if slices.Equal(before, m.Entries) {
		return nil
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.writeAtomic(ManifestFile, append(data, '\n'), 0o644)
	return err
}
