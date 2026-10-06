package git

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Unpack writes the tar archive r into dest, an existing directory that is
// normally new and empty: Unpack never replaces a file. Files keep their
// modes, directory entries get 0755, whatever the umask, and symlinks are
// kept. It refuses an entry whose path leaves dest, a symlink that points
// outside dest or loops, and any entry that is not a directory, regular file
// or symlink.
// After the archive ends it reads r to EOF, so an error from whatever
// produced r (such as git exiting non-zero after Archive) is returned too.
//
// On error dest holds a partial tree, which the caller removes.
func Unpack(r io.Reader, dest string) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var links []string
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue // git archive records the commit id here
		}
		name, err := entryName(hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if name == "." {
				continue
			}
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			if err := root.Chmod(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(root, name, hdr.FileInfo().Mode().Perm(), tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := symlinkStaysInside(name, hdr.Linkname); err != nil {
				return err
			}
			if err := mkParent(root, name); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			links = append(links, name)
		default:
			return fmt.Errorf("archive entry %q: unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("reading archive: %w", err)
	}

	// The lexical check in symlinkStaysInside can't see through the link's
	// own directory: with self -> ".", a link at "self/x" to ".." looks local
	// but isn't. Resolving each link through root, which refuses to leave
	// dest, can.
	for _, name := range links {
		_, err := root.Stat(name)
		if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue // resolves inside dest, or dangles
		}
		target, _ := root.Readlink(name)
		return fmt.Errorf("archive symlink %q -> %q: %w", name, target, err)
	}
	return nil
}

// entryName turns a tar entry name into a slash-separated path inside the
// destination, or refuses it.
func entryName(name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if name == "" || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", fmt.Errorf("archive entry %q is outside the destination", name)
	}
	return clean, nil
}

// symlinkStaysInside refuses a target that is absolute, climbs above the
// destination from the link's directory, or has ".." after a name. "a/.."
// is only lexically a no-op: a may be a symlink, or missing until something
// creates it.
func symlinkStaysInside(name, target string) error {
	ok := target != "" && !path.IsAbs(target)
	up, named := 0, false
	for part := range strings.SplitSeq(target, "/") {
		switch part {
		case "", ".":
		case "..":
			ok = ok && !named
			up++
		default:
			named = true
		}
	}
	if !ok || up > strings.Count(name, "/") {
		return fmt.Errorf("archive symlink %q -> %q points outside the destination", name, target)
	}
	return nil
}

func mkParent(root *os.Root, name string) error {
	if dir := path.Dir(name); dir != "." {
		return root.MkdirAll(dir, 0o755)
	}
	return nil
}

// writeFile creates name, which must not exist yet, with mode perm. The
// mode is set explicitly so the tree doesn't depend on the user's umask.
func writeFile(root *os.Root, name string, perm fs.FileMode, r io.Reader) error {
	if err := mkParent(root, name); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Chmod(perm)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
