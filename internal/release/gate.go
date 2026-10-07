package release

import (
	"context"
	"fmt"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// A SelfUpdater replaces the running binary with another release (ticket
// 060). The gate calls it only when the user asked: on a TTY by accepting
// the offer, otherwise with --self-update.
type SelfUpdater interface {
	// SelfUpdate installs version and re-executes the command with its
	// original arguments, so it returns only on failure. A binary it may
	// not replace (package-managed, not writable) is an error that says
	// what to do instead.
	SelfUpdate(ctx context.Context, version string) error
}

// GateOptions are the compatibility gate's inputs (§8, D12).
type GateOptions struct {
	// CLIVersion is the running pic-sure's version, "dev" for a dev build.
	CLIVersion string
	// Compat is release.cli_compat.
	Compat stack.CLICompat
	// SelfUpdate is --self-update.
	SelfUpdate bool
	// IgnoreCLIVersion is --ignore-cli-version: skip the gate.
	IgnoreCLIVersion bool
	// Confirm asks the user a yes/no question. Set it only when the
	// command may prompt (a TTY); nil means no TTY.
	Confirm func(ctx context.Context, question string) (bool, error)
	// Updater replaces the binary. Nil makes a newer PSCLI exit 5 with
	// instructions.
	Updater SelfUpdater
	// Command is the command line to retry with, for messages, such as
	// "pic-sure update".
	Command string
	// Sink and Step receive the gate's warnings.
	Sink events.Sink
	Step string
}

// Gate is the CLI compatibility gate: it compares the build-spec's PSCLI
// with the running CLI before any stack mutation. It returns nil to
// proceed and an exit-5 error to stop. A newer PSCLI may self-update, which
// on success doesn't return.
func (r *Release) Gate(ctx context.Context, opts GateOptions) error {
	want, ok := r.Spec.Ref(catalog.CLISpecKey)
	if !ok {
		warn(opts.Sink, opts.Step, "the build-spec at release-control %s isn't CLI-aware (no %s entry); "+
			"it wasn't validated with any pic-sure release", short(r.Commit), catalog.CLISpecKey)
		return nil
	}
	order, comparable := stack.CompareVersions(want, opts.CLIVersion)
	if !comparable {
		if !isVersion(opts.CLIVersion) {
			warn(opts.Sink, opts.Step, "pic-sure %s is a development build; treating it as the release's pic-sure %s",
				opts.CLIVersion, want)
		} else {
			warn(opts.Sink, opts.Step, "the build-spec's %s %q isn't a version; skipping the CLI version check",
				catalog.CLISpecKey, want)
		}
		return nil
	}
	if order != 0 && opts.IgnoreCLIVersion {
		warn(opts.Sink, opts.Step, "release-control %s was validated with pic-sure %s, not %s; "+
			"continuing because of --ignore-cli-version", short(r.Commit), want, opts.CLIVersion)
		return nil
	}
	switch {
	case order < 0:
		if opts.Compat == stack.CompatStrict {
			return exitcode.Incompatible("release-control %s was validated with pic-sure %s, older than this pic-sure %s, "+
				"and release.cli_compat is strict; set it to warn or pass --ignore-cli-version",
				short(r.Commit), want, opts.CLIVersion)
		}
		warn(opts.Sink, opts.Step, "release-control %s was validated with pic-sure %s, older than this pic-sure %s; "+
			"this pairing wasn't tested", short(r.Commit), want, opts.CLIVersion)
		return nil
	case order > 0:
		return r.newerCLI(ctx, want, opts)
	}
	return nil
}

// newerCLI handles a build-spec that names a newer CLI than this one.
func (r *Release) newerCLI(ctx context.Context, want string, opts GateOptions) error {
	gap := fmt.Sprintf("release-control %s needs pic-sure %s; this is pic-sure %s", short(r.Commit), want, opts.CLIVersion)
	retry := opts.Command
	if retry == "" {
		retry = "the command"
	}
	if opts.Updater == nil {
		return exitcode.Incompatible("%s. Install pic-sure %s, then run %s again, "+
			"or pass --ignore-cli-version to use this one", gap, want, retry)
	}
	update := opts.SelfUpdate
	if !update && opts.Confirm != nil {
		yes, err := opts.Confirm(ctx, gap+". Update pic-sure now?")
		if err != nil {
			return err
		}
		if !yes {
			return exitcode.Incompatible("%s. Run `pic-sure self-update --to %s`, "+
				"or pass --ignore-cli-version to use this one", gap, want)
		}
		update = true
	}
	if !update {
		return exitcode.Incompatible("%s. Run %s --self-update to update and continue, "+
			"or `pic-sure self-update --to %s` first, or pass --ignore-cli-version to use this one", gap, retry, want)
	}
	if err := opts.Updater.SelfUpdate(ctx, want); err != nil {
		return err
	}
	// SelfUpdate re-executes on success, so returning means this old binary
	// is still running and mustn't go on to change the stack.
	return exitcode.Incompatible("pic-sure was updated to %s; run %s again", want, retry)
}

// isVersion reports whether CompareVersions can order v.
func isVersion(v string) bool {
	_, ok := stack.CompareVersions(v, v)
	return ok
}
