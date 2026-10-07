package dashboard

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/contract"
)

// errNotImplemented is what every dashboard read reports until ticket 040
// polls the compose adapter (017) and the v2 status report (027) in-process.
var errNotImplemented = errors.New("not implemented in v2 yet (ticket 040)")

const (
	servicesInterval = 2 * time.Second
	statusInterval   = 15 * time.Second
)

// Log-follower restart backoff: a dead follower is restarted after a delay
// that doubles on each consecutive failed restart, capped at logRetryMax, so a
// service whose `compose logs -f` keeps failing is retried ever less often
// instead of every servicesInterval. The delay resets once a session delivers
// real lines.
const (
	logRetryBase = 2 * time.Second
	logRetryMax  = 30 * time.Second
)

// nextLogRetryDelay computes the next backoff from the previous one: it starts
// at logRetryBase and doubles up to logRetryMax. Pure so tests can assert the
// schedule grows without sleeping.
func nextLogRetryDelay(prev time.Duration) time.Duration {
	if prev < logRetryBase {
		return logRetryBase
	}
	if d := prev * 2; d < logRetryMax {
		return d
	}
	return logRetryMax
}

type (
	servicesTickMsg struct{}
	statusTickMsg   struct{}

	servicesMsg struct {
		services []contract.ComposeService
		err      error
	}
	statusMsg struct {
		status *contract.Status
		err    error
	}

	logLinesMsg struct {
		sessionID int
		lines     []string
	}
	logClosedMsg struct {
		sessionID int
	}
	// logRetryMsg fires after the backoff delay to restart a dead follower for
	// seq's service (stamped with the logSeq the closure observed so a restart
	// scheduled for an old service is discarded once the user has switched).
	logRetryMsg struct {
		seq int
	}
)

// logRetry schedules a follower restart after delay, stamped with seq.
func logRetry(seq int, delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg { return logRetryMsg{seq: seq} })
}

func servicesTick() tea.Cmd {
	return tea.Tick(servicesInterval, func(time.Time) tea.Msg { return servicesTickMsg{} })
}

func statusTick() tea.Cmd {
	return tea.Tick(statusInterval, func(time.Time) tea.Msg { return statusTickMsg{} })
}

// pollCmd builds a context-bound `bash <script> <args>` poll. No v2 code
// calls it: it stays so TestPollCmdNotWedgedByOrphanGrandchild keeps
// compiling as the model for the exec runner's grandchild handling (ticket
// 003). Ticket 040 deletes both.
//
// The script's own context kill only reaches bash, but a script that runs
// `docker compose` as a non-exec'd child hands the grandchild the stdout
// pipe. On a context timeout CommandContext would kill bash alone; the
// orphaned docker process keeps the write end open and Wait blocks on the
// I/O-copy goroutine until EOF — forever if the daemon is hung, silently
// wedging the poll. So run the poll in its own process group and bring the
// whole group down on cancel; WaitDelay is a backstop in case a process
// escapes the group.
func pollCmd(ctx context.Context, root, script string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(root, script)}, args...)...)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// SIGKILL, not SIGTERM: a hung-daemon grandchild may ignore TERM, and polls have nothing to drain.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func pollServices(string) tea.Cmd {
	return func() tea.Msg { return servicesMsg{err: errNotImplemented} }
}

func pollStatus(string) tea.Cmd {
	return func() tea.Msg { return statusMsg{err: errNotImplemented} }
}
