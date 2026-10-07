package cli

import (
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// Every secret the stack package loads or generates goes to the log
// redactor before anything can log it (§6.3).
func init() { stack.SetSecretRegistrar(log.RegisterSecrets) }

// readOnlyCommands write a run log file only at --log-level debug, so that
// polling them never fills .pic-sure/logs (spec §6.1). Keys are command
// paths without "pic-sure ".
var readOnlyCommands = map[string]bool{
	"status": true, "ps": true, "logs": true, "doctor": true,
	"config show": true, "config get": true, "version": true,
}

// startRunLog starts the logging for a command run. markRunning calls it as
// the command's RunE starts, once the flags are parsed, and Run's
// endRunLog closes it.
func (a *App) startRunLog(cmd *cobra.Command, args []string) {
	level, err := log.ParseLevel(a.Global.LogLevel)
	if err != nil {
		level = slog.LevelInfo // only for an App built without the flag
	}
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	a.runLog = log.New(log.Options{
		Level:  level,
		Stderr: a.Stderr,
		File:   level <= slog.LevelDebug || !readOnlyCommands[path],
	})
	var flags []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		flags = append(flags, "--"+f.Name+"="+f.Value.String())
	})
	a.runLog.Logger().Debug("pic-sure run",
		"version", a.Info.Version, "commit", a.Info.Commit,
		"os", runtime.GOOS, "arch", runtime.GOARCH,
		"command", cmd.CommandPath(), "flags", flags, "args", args)
}

// openRunLog starts the run's log file in st's .pic-sure/logs. openStack
// calls it once the stack has passed the version gate. It does nothing for
// a read-only command below --log-level debug, and a failure is a warning,
// never the command's failure.
func (a *App) openRunLog(st *stack.Stack) {
	if a.runLog == nil {
		return
	}
	if _, err := a.runLog.OpenFile(runLogStore{st}, time.Now()); err != nil {
		a.runLog.Logger().Warn("can't write the run log", "dir", st.Dir, "err", err)
	}
}

// runLogStore is the stack as log.Run uses it: a run log is the CLI's if
// the manifest lists it.
type runLogStore struct{ *stack.Stack }

func (s runLogStore) Owns(rel string) bool {
	m, err := s.Manifest()
	return err == nil && m.Has(rel)
}

// endRunLog records how the run ended and closes its log file.
func (a *App) endRunLog(err error) {
	if a.runLog == nil {
		return
	}
	attrs := []any{"code", exitcode.FromError(err)}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	a.runLog.Logger().Debug("pic-sure exit", attrs...)
	_ = a.runLog.Close()
	a.runLog = nil
}

// newLogger returns the run's logger. Outside a command's RunE there is no
// run, and the logger discards everything.
func (a *App) newLogger() *slog.Logger {
	if a.runLog == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.runLog.Logger()
}

// levelFlag is the --log-level value. It accepts only what log.ParseLevel
// does, so a bad level is a usage error, and stores it in lower case.
type levelFlag struct{ level *string }

func (f levelFlag) String() string {
	if f.level == nil { // pflag calls String on a zero value when printing help
		return ""
	}
	return *f.level
}

func (f levelFlag) Set(s string) error {
	if _, err := log.ParseLevel(s); err != nil {
		return err
	}
	*f.level = strings.ToLower(s)
	return nil
}

func (levelFlag) Type() string { return "level" }
