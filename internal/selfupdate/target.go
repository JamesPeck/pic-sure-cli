package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemPrefixes are directories an OS package manager owns. A binary under
// one is updated with that package manager, not replaced in place.
var systemPrefixes = []string{
	"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/lib/", "/usr/lib64/",
	"/usr/libexec/", "/usr/share/", "/nix/store/", "/snap/",
}

// target is the binary an update replaces.
type target struct {
	// Path is the real file: symlinks resolved, so an update replaces what
	// a symlink points at rather than the link.
	Path string
	// Mode is the file's permission bits, kept by the replacement.
	Mode os.FileMode
}

// refusal is why the binary at Path can't be replaced in place. Its
// message says what to do instead.
type refusal struct {
	Path   string
	Reason string
	Advice string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("can't update pic-sure at %s: %s; %s", r.Path, r.Reason, r.Advice)
}

// findTarget resolves exe and checks that it may be replaced: not under a
// package manager's prefix, and in a directory this user can write to.
// version is the release being installed, for the advice.
func findTarget(exe, version string) (target, error) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return target{}, fmt.Errorf("finding the pic-sure binary: %w", err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return target{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return target{}, fmt.Errorf("finding the pic-sure binary: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return target{}, fmt.Errorf("finding the pic-sure binary: %s isn't a regular file", real)
	}
	if r := managed(real, version); r != nil {
		return target{}, r
	}
	if err := checkWritable(filepath.Dir(real)); err != nil {
		return target{}, &refusal{
			Path:   real,
			Reason: fmt.Sprintf("you can't create files in %s (%v)", filepath.Dir(real), errors.Unwrap(err)),
			Advice: fmt.Sprintf("run `sudo pic-sure self-update%s`, or reinstall with %s",
				toFlag(version), installCommand(version, "--bin-dir DIR")),
		}
	}
	return target{Path: real, Mode: fi.Mode().Perm()}, nil
}

// managed returns a refusal if a package manager owns path.
func managed(path, version string) *refusal {
	slash := filepath.ToSlash(path)
	if strings.Contains(slash, "/Cellar/") || strings.Contains(slash, "/Caskroom/") {
		return &refusal{Path: path, Reason: "Homebrew manages it",
			Advice: "update it with `brew upgrade pic-sure`"}
	}
	for _, p := range systemPrefixes {
		if strings.HasPrefix(slash, p) {
			return &refusal{Path: path, Reason: "it is in a system directory a package manager owns",
				Advice: "update it with that package manager, or install pic-sure in your home directory with " +
					installCommand(version, "")}
		}
	}
	return nil
}

// checkWritable reports whether a file can be created, and so renamed into
// place, in dir.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".pic-sure-update-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// installCommand is the install.sh one-liner for version (the latest when
// empty), with extra options.
func installCommand(version, extra string) string {
	args := extra
	if version != "" {
		args = strings.TrimSpace(args + " --version " + version)
	}
	cmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | bash", DefaultRepo)
	if args != "" {
		cmd += " -s -- " + args
	}
	return "`" + cmd + "`"
}

// toFlag is " --to VERSION", or nothing for the latest release.
func toFlag(version string) string {
	if version == "" {
		return ""
	}
	return " --to " + version
}
