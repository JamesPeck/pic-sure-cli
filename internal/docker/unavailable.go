package docker

import (
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
)

// What to tell a user whose docker is missing or whose daemon doesn't
// answer. doctor and the CLI's exit-3 mapping (spec §10.4) both say it.
const (
	InstallHint = "install Docker Desktop, Colima, OrbStack or Docker Engine"
	StartHint   = "Start Docker Desktop, Colima or OrbStack, or the docker service, and check `docker context ls`."
)

// unreachableRE matches the docker CLI's and compose's errors for a daemon
// they couldn't connect to: a stopped Docker Desktop, or a DOCKER_HOST or
// context pointing nowhere.
var unreachableRE = regexp.MustCompile(`(?i)cannot connect to the docker daemon|failed to connect to the docker api|error during connect`)

// IsMissing reports whether err is the docker executable not being found.
func IsMissing(err error) bool {
	var ee *exec.Error
	return errors.As(err, &ee) && filepath.Base(ee.Name) == "docker" && errors.Is(ee.Err, exec.ErrNotFound)
}

// IsUnreachable reports whether err is the docker CLI failing to reach the
// daemon: an error matching ErrDaemonUnreachable, or one quoting docker's
// own connection error.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrDaemonUnreachable) {
		return true
	}
	var ee *ExitError
	if errors.As(err, &ee) && unreachableRE.Match(ee.Stderr) {
		return true
	}
	return unreachableRE.MatchString(err.Error())
}
