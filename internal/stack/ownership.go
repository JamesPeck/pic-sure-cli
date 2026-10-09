package stack

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

// Claim is whose a Docker resource is, by the ownership rule (§6.1).
type Claim int

const (
	// Own is a resource of this stack.
	Own Claim = iota
	// Moved is a resource of this stack still labelled with the directory
	// the stack was moved from. Docker can't relabel a volume, so it keeps
	// that label; the stack adopts it.
	Moved
	// Foreign is another stack's resource, or one pic-sure didn't make.
	Foreign
)

// Owner applies the ownership rule (§6.1) to a resource's labels, for the
// stack with ID id in dir:
//   - a different stack-id is foreign;
//   - this ID with another stack-dir is foreign if that directory still
//     holds a stack with this ID (the original of a copy) or can't be
//     read, and moved otherwise;
//   - no stack-id (made before stacks had one) is this stack's only if its
//     stack-dir is dir.
func Owner(id, dir string, labels map[string]string) Claim {
	labelDir := labels[LabelStackDir]
	labelID := labels[LabelStackID]
	switch {
	case labelID == "":
		if sameDir(labelDir, dir) {
			return Own
		}
		return Foreign
	case labelID != id:
		return Foreign
	case sameDir(labelDir, dir):
		return Own
	case !filepath.IsAbs(labelDir):
		return Foreign
	}
	other, err := IDAt(labelDir)
	if err != nil || other == id {
		return Foreign
	}
	return Moved
}

// sameDir reports whether the labelled directory is dir, also through a
// symlink left at the old path after a move.
func sameDir(labelDir, dir string) bool {
	if labelDir == "" || labelDir == dir {
		return labelDir != ""
	}
	r, err := filepath.EvalSymlinks(labelDir)
	return err == nil && r == dir
}

// OwnerName describes, for a message, the stack a resource with labels
// belongs to: "stack demo in /srv/demo", or what is known of it.
func OwnerName(labels map[string]string) string {
	name, dir := labels[LabelStack], labels[LabelStackDir]
	switch {
	case dir != "" && name != "":
		return fmt.Sprintf("stack %s in %s", name, dir)
	case dir != "":
		return "the stack in " + dir
	case name != "":
		return fmt.Sprintf("a stack %s with no %s label", name, LabelStackDir)
	}
	return "something other than pic-sure"
}

// IDAt returns the stack ID in dir's state.json, "" if dir holds no stack
// state. An error means dir couldn't be read, which the ownership rule
// counts as another stack's.
func IDAt(dir string) (string, error) {
	st, err := PeekState(dir)
	if st == nil {
		return "", err
	}
	return st.StackID, nil
}

// ID returns the stack's ID from state.json, "" before it has one.
func (s *Stack) ID() string {
	st, err := s.LoadState()
	if err != nil {
		return ""
	}
	return st.StackID
}

// EnsureID returns the stack's ID, giving a stack without one a new ID
// from rand first. Call it holding the stack lock. A stack with no
// state.json yet (init died before writing it) has no ID: "" and no error.
func (s *Stack) EnsureID(rand io.Reader) (string, error) {
	st, err := s.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.StackID != "" {
		return st.StackID, nil
	}
	if st.StackID, err = NewID(rand); err != nil {
		return "", err
	}
	return st.StackID, s.SaveState(st)
}

// NewID returns a random stack ID: 16 bytes from rand, in hex.
func NewID(rand io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand, b); err != nil {
		return "", fmt.Errorf("generating the stack ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
