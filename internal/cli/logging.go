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

// quietRunLog reports whether cmd writes a run log file only at
// --log-level debug, so that polling it never fills .pic-sure/logs (spec
// §6.1): the version gate's read-only commands (commandClass).
func quietRunLog(cmd *cobra.Command) bool {
	return commandClass(cmd) == stack.ReadOnly
}

// startRunLog starts the logging for a command run. markRunning calls it as
// the command's RunE starts, once the flags are parsed, and Run's
// endRunLog closes it.
func (a *App) startRunLog(cmd *cobra.Command, args []string) {
	level, err := log.ParseLevel(a.Global.LogLevel)
	if err != nil {
		level = slog.LevelInfo // only for an App built without the flag
	}
	path := commandName(cmd)
	a.runLog = log.New(log.Options{
		Level:  level,
		Stderr: logStderr{a},
		File:   level <= slog.LevelDebug || !quietRunLog(cmd),
	})
	var flags []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		// init's admin email is personal data, redacted like a secret. So is
		// a secret given to --set, which init refuses only later.
		if f.Name == "admin-email" {
			log.RegisterSecrets(f.Value.String())
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok && f.Name == "set" {
			for _, kv := range sv.GetSlice() {
				key, v, _ := strings.Cut(kv, "=")
				if stack.PrivateKey(key) {
					registerConfigValue(v)
					kv = key + "=" + log.Redacted
				}
				flags = append(flags, "--set="+kv)
			}
			return
		}
		flags = append(flags, "--"+f.Name+"="+f.Value.String())
	})
	a.runLog.Logger().Debug("pic-sure run",
		append([]any{
			"version", a.Info.Version, "commit", a.Info.Commit,
			"os", runtime.GOOS, "arch", runtime.GOARCH,
			"command", cmd.CommandPath(), "flags", flags,
		}, logArgs(path, args)...)...)
}

// logArgs is the attrs that record args in the run log. `config set KEY
// VALUE` keeps the key and redacts a private key's value. It also registers
// the value with the redactor, since config set may yet refuse it, and then
// it never reaches secrets.yaml, where the registry would otherwise find it.
// `compose -- ARGS` records only the compose subcommand and how many
// arguments follow it, since they may hold a password typed on the
// command line.
func logArgs(path string, args []string) []any {
	switch {
	case path == "compose":
		sub, after := "", len(args)
		if i := composeSubcommand(args); i >= 0 {
			sub, after = args[i], len(args)-i-1
		}
		return []any{"compose_command", sub, "compose_args", after}
	case path == "config set" && len(args) >= 2 && stack.PrivateKey(args[0]):
		registerConfigValue(args[1])
		return []any{"args", append([]string{args[0], log.Redacted}, args[2:]...)}
	}
	return []any{"args", args}
}

// registerConfigValue registers a private key's value with the redactor,
// unless it is a boolean: redacting every "true" and "false" would wreck the
// logs.
func registerConfigValue(v string) {
	if !strings.EqualFold(v, "true") && !strings.EqualFold(v, "false") {
		log.RegisterSecrets(v)
	}
}

// openRunLog starts the run's log file in st's .pic-sure/logs. openStack
// calls it once the stack has passed the version gate. It does nothing for
// a read-only command below --log-level debug, and a failure is a warning,
// never the command's failure.
func (a *App) openRunLog(st *stack.Stack) {
	if a.runLog == nil {
		return
	}
	path, err := a.runLog.OpenFile(runLogStore{st}, time.Now())
	if err != nil {
		a.runLog.Logger().Warn("can't write the run log", "dir", st.Dir, "err", err)
		return
	}
	a.runLogPath = path
}

// logStderr is where log records for stderr go: to the full-screen TUI's
// init as events while it runs one, through the TUI renderer while the run
// has one, so they print above its frame, else to stderr.
type logStderr struct{ a *App }

func (w logStderr) Write(b []byte) (int, error) {
	if l := w.a.tuiLog.Load(); l != nil {
		return l.Write(b)
	}
	if r := w.a.tuiOut.Load(); r != nil {
		return r.Write(b)
	}
	return w.a.stderr().Write(b)
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
