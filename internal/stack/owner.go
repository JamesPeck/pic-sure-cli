package stack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// ErrNotOwned is wrapped by the error Find returns when the stack it found
// without --stack belongs to another user (§6.1).
var ErrNotOwned = errors.New("stack belongs to another user")

// Seams for tests, which can't chown without root.
var (
	fileOwner = func(_ string, fi fs.FileInfo) int { return int(fi.Sys().(*syscall.Stat_t).Uid) }
	euid      = os.Geteuid
	sudoUID   = func() string { return os.Getenv("SUDO_UID") }
)

// checkOwner returns an exit-3 error wrapping ErrNotOwned unless the current
// user owns dir and its pic-sure.yaml. Another user could have planted a
// stack in a shared directory such as /tmp, and compose would then run their
// services with the docker group's root-equivalent access (the class of git's
// CVE-2022-24765). Under sudo, root also trusts the invoking user's stacks,
// as git does.
func checkOwner(dir string) error {
	for _, p := range []string{dir, filepath.Join(dir, ConfigFile)} {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if uid := fileOwner(p, fi); !trustedUID(uid) {
			return exitcode.Precondition("%w: %s is owned by %s, not you; pass --stack %q if you trust it",
				ErrNotOwned, p, ownerName(uid), dir)
		}
	}
	return nil
}

func trustedUID(uid int) bool {
	me := euid()
	if uid == me {
		return true
	}
	if me != 0 {
		return false
	}
	s, err := strconv.Atoi(sudoUID())
	return err == nil && s == uid
}

// ownerName is uid's user name and number, or just the number when it has no
// name.
func ownerName(uid int) string {
	id := strconv.Itoa(uid)
	if u, err := user.LookupId(id); err == nil && u.Username != "" {
		return fmt.Sprintf("%s (uid %s)", u.Username, id)
	}
	return "uid " + id
}
