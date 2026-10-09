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

// socketErrors are the docker CLI's and compose's errors for a daemon socket
// they couldn't reach or open: a stopped Docker Desktop, a DOCKER_HOST or
// context pointing nowhere, or a user outside the docker group.
const socketErrors = `cannot connect to the docker daemon|failed to connect to the docker api|(got )?permission denied while trying to connect to the docker|error during connect: `

var (
	unreachableRE    = regexp.MustCompile(`(?i)` + socketErrors)
	composeMissingRE = regexp.MustCompile(`(?i)unknown command: docker compose|'compose' is not a docker command`)
)

// IsMissing reports whether err is the docker executable not being found.
func IsMissing(err error) bool {
	var ee *exec.Error
	return errors.As(err, &ee) && filepath.Base(ee.Name) == "docker" && errors.Is(ee.Err, exec.ErrNotFound)
}

// IsUnreachable reports whether err is the docker CLI failing to reach the
// daemon: an error matching ErrDaemonUnreachable, or one quoting docker's
// own connection error (an *ExitError quotes stderr's last line).
func IsUnreachable(err error) bool {
	return err != nil && (errors.Is(err, ErrDaemonUnreachable) || unreachableRE.MatchString(err.Error()))
}

// IsComposeMissing reports whether err is docker saying it has no compose
// plugin.
func IsComposeMissing(err error) bool {
	return err != nil && composeMissingRE.MatchString(err.Error())
}
