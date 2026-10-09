package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/selfupdate"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// initFromTUI is tui.Options.Init: init run in-process for the TUI's setup
// wizard, or to resume a stack init didn't finish. Its events go to
// req.Sink, and so do the run log's stderr records, so nothing writes over
// the TUI. The compatibility gate offers its self-update through
// req.Confirm, and then installs the new pic-sure without re-running it.
func (a *App) initFromTUI(ctx context.Context, req tui.InitRequest) (tui.InitResult, error) {
	// Registering the global flags again resets a.Global to the defaults.
	global := a.Global
	cmd, _, err := newRootCmd(a).Find([]string{"init"})
	a.Global = global
	if err != nil {
		return tui.InitResult{}, err
	}
	r := &initRun{
		a: a, cmd: cmd, dir: req.Dir,
		supplied:    req.Secrets,
		confirm:     req.Confirm,
		installOnly: true,
		gateCommand: "pic-sure",
	}
	if req.Config != nil {
		if r.cfg, err = req.Config.Config(); err != nil {
			return tui.InitResult{}, err
		}
		r.doc = req.Config
		r.httpPort, r.httpsPort = r.cfg.Network.HTTPPort, r.cfg.Network.HTTPSPort
	}

	saved := a.out
	a.out = &output{mode: modeTUI, sink: &runSink{Sink: req.Sink}}
	a.tuiLog.Store(&logEvents{redactingSink{req.Sink}})
	defer func() {
		a.out = saved
		a.tuiLog.Store(nil)
	}()
	summary, err := r.run(ctx)
	res := tui.InitResult{LogPath: a.runLogPath}
	if err != nil {
		// As reportError does: an error can quote a secret.
		return res, errors.New(log.Redact(err.Error()))
	}
	var b strings.Builder
	if summary.AlreadyInitialized {
		b.WriteString("Stack " + summary.Stack + " is already initialised.\n")
	} else if err := writeInitSummary(&b, summary); err != nil {
		return res, err
	}
	res.Summary = b.String()
	return res, nil
}

// installOnly is the gate's self-updater in the TUI: it installs the new
// pic-sure but doesn't re-run, which would restart the TUI and lose its
// state, such as the wizard's answers. The gate then says to run pic-sure
// again.
type installOnly struct{ u *selfupdate.Updater }

func (i installOnly) SelfUpdate(ctx context.Context, version string) error {
	_, err := i.u.Install(ctx, version)
	return err
}

// logEvents writes log records as Log events, one per line.
type logEvents struct{ sink events.Sink }

func (l *logEvents) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		l.sink.Emit(events.Log{Stream: events.StreamStderr, Line: line})
	}
	return len(b), nil
}

var _ io.Writer = (*logEvents)(nil)

// wizardDefaults is tui.Options.Defaults: the default config, named after
// dir, with the ports init would choose now (§6.5: 80/443 when free, else
// the first free pair from 8080/8443), passing over the ports registered
// stacks have.
func wizardDefaults(dir string) stack.Config {
	cfg := stack.DefaultConfig()
	cfg.Name = suggestName(filepath.Base(dir))
	reserved, _ := reservedInDefaultCache(dir)
	h := ops.ReservingHost{Host: systemHost{}, Reserved: reserved}
	hp, sp, err := ops.ChoosePorts(h, 0, 0, false)
	if err != nil {
		hp, sp, err = ops.ChoosePorts(h, 0, 0, true)
	}
	if err == nil {
		cfg.Network.HTTPPort, cfg.Network.HTTPSPort = hp, sp
		if base, err := ops.ChooseDevPortsBase(h, hp, sp); err == nil {
			cfg.Network.DevPorts.Base = base
		}
	}
	return cfg
}

// suggestName makes a valid stack name of s: lower case, other characters
// as -, starting with a letter or digit; "picsure" when nothing is left.
func suggestName(s string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, s)
	if name = strings.TrimLeft(name, "-_"); name == "" {
		return "picsure"
	}
	return name
}
