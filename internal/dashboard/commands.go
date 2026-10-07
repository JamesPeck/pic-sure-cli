package dashboard

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

const (
	servicesInterval = 2 * time.Second
	statusInterval   = 15 * time.Second

	// servicesTimeout bounds one services read, opening the stack
	// included, so a hung daemon can't wedge the pane: only one poll runs
	// at a time.
	servicesTimeout = 10 * time.Second
	// statusTimeout bounds one status report, which inspects images and
	// checks the migrations.
	statusTimeout = 60 * time.Second
	// deepTimeout bounds `status --deep`, whose probes take up to 25 s each.
	deepTimeout = 2 * time.Minute
)

// Log-follower restart backoff: a follower that ends is restarted after a
// delay that doubles, up to logRetryMax, while followers keep ending within
// logRanLong of starting (an error, a stopped container). One that ran
// longer resets it.
const (
	logRetryBase = 2 * time.Second
	logRetryMax  = 30 * time.Second
	logRanLong   = 30 * time.Second
)

// nextLogRetryDelay computes the next backoff from the previous one: it starts
// at logRetryBase and doubles up to logRetryMax.
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
		services []ops.StatusService
		err      error
	}
	statusMsg struct {
		report *ops.StatusReport
		err    error
	}
	// deepMsg is a deep check's result. gen is the deepGen it started
	// under: an action since then makes it stale.
	deepMsg struct {
		gen    int
		report *ops.StatusReport
		err    error
		at     time.Time
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

func pollServices(ctx context.Context, b Backend) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, servicesTimeout)
		defer cancel()
		services, err := b.Services(ctx)
		return servicesMsg{services: services, err: err}
	}
}

func pollStatus(ctx context.Context, b Backend) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, statusTimeout)
		defer cancel()
		report, err := b.Status(ctx, false)
		return statusMsg{report: report, err: err}
	}
}

func probeDeep(ctx context.Context, b Backend, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, deepTimeout)
		defer cancel()
		report, err := b.Status(ctx, true)
		return deepMsg{gen: gen, report: report, err: err, at: time.Now()}
	}
}
