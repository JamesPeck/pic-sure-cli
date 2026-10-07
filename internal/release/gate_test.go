package release_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

type fakeUpdater struct {
	got []string
	err error
}

func (u *fakeUpdater) SelfUpdate(_ context.Context, version string) error {
	u.got = append(u.got, version)
	return u.err
}

func releaseWith(t *testing.T, pscli string) *release.Release {
	t.Helper()
	cli := ""
	if pscli != "" {
		cli = `,{"project_job_git_key":"PSCLI","git_hash":"` + pscli + `"}`
	}
	spec, err := release.ParseBuildSpec([]byte(`{"application":[{"project_job_git_key":"PSA","git_hash":"main"}` + cli + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return &release.Release{Commit: strings.Repeat("ab", 20), Spec: spec}
}

func TestGateMatrix(t *testing.T) {
	type tty int
	const (
		noTTY tty = iota
		ttyYes
		ttyNo
	)
	tests := []struct {
		name       string
		pscli      string // "" = no PSCLI entry
		cli        string
		strict     bool
		tty        tty
		selfUpdate bool
		ignore     bool
		noUpdater  bool
		wantCode   int // 0 = proceed
		wantUpdate bool
		wantWarn   string
		wantErr    string
	}{
		{name: "missing", pscli: "", cli: "v2.0.0", wantWarn: "isn't CLI-aware"},
		{name: "missing strict", pscli: "", cli: "v2.0.0", strict: true, wantWarn: "isn't CLI-aware"},
		{name: "equal", pscli: "v2.0.0", cli: "v2.0.0"},
		{name: "equal ignoring v and build", pscli: "v2.0.0", cli: "2.0.0+abc"},
		{name: "older warns", pscli: "v2.0.0", cli: "v2.1.0", wantWarn: "wasn't tested"},
		{name: "older on a TTY warns", pscli: "v2.0.0", cli: "v2.1.0", tty: ttyYes, wantWarn: "wasn't tested"},
		{name: "older strict", pscli: "v2.0.0", cli: "v2.1.0", strict: true, wantCode: exitcode.CodeIncompatible, wantErr: "strict"},
		{name: "older strict ignored", pscli: "v2.0.0", cli: "v2.1.0", strict: true, ignore: true, wantWarn: "--ignore-cli-version"},
		{name: "newer without a TTY", pscli: "v2.1.0", cli: "v2.0.0", wantCode: exitcode.CodeIncompatible, wantErr: "pic-sure update --self-update"},
		{name: "newer without a TTY, --self-update", pscli: "v2.1.0", cli: "v2.0.0", selfUpdate: true, wantUpdate: true},
		{name: "newer on a TTY, accepted", pscli: "v2.1.0", cli: "v2.0.0", tty: ttyYes, wantUpdate: true},
		{name: "newer on a TTY, declined", pscli: "v2.1.0", cli: "v2.0.0", tty: ttyNo, wantCode: exitcode.CodeIncompatible, wantErr: "self-update --to v2.1.0"},
		{name: "newer on a TTY with --self-update skips the prompt", pscli: "v2.1.0", cli: "v2.0.0", tty: ttyNo, selfUpdate: true, wantUpdate: true},
		{name: "newer ignored", pscli: "v2.1.0", cli: "v2.0.0", ignore: true, selfUpdate: true, wantWarn: "--ignore-cli-version"},
		{name: "newer strict ignored", pscli: "v2.1.0", cli: "v2.0.0", strict: true, ignore: true, wantWarn: "--ignore-cli-version"},
		{name: "newer before self-update exists", pscli: "v2.1.0", cli: "v2.0.0", selfUpdate: true, tty: ttyYes, noUpdater: true,
			wantCode: exitcode.CodeIncompatible, wantErr: "Install pic-sure v2.1.0"},
		{name: "prerelease is older than release", pscli: "v2.0.0", cli: "v2.0.0-rc.1", wantCode: exitcode.CodeIncompatible},
		{name: "dev build", pscli: "v2.1.0", cli: "dev", strict: true, wantWarn: "development build"},
		{name: "dev build, no TTY", pscli: "v1.0.0", cli: "dev", wantWarn: "development build"},
		{name: "PSCLI not a version", pscli: "main", cli: "v2.0.0", strict: true, wantWarn: "isn't a version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec events.Recorder
			u := &fakeUpdater{}
			var prompts []string
			opts := release.GateOptions{
				CLIVersion:       tt.cli,
				Compat:           stack.CompatWarn,
				SelfUpdate:       tt.selfUpdate,
				IgnoreCLIVersion: tt.ignore,
				Updater:          u,
				Command:          "pic-sure update",
				Sink:             &rec,
				Step:             "release",
			}
			if tt.strict {
				opts.Compat = stack.CompatStrict
			}
			if tt.noUpdater {
				opts.Updater = nil
			}
			if tt.tty != noTTY {
				opts.Confirm = func(_ context.Context, q string) (bool, error) {
					prompts = append(prompts, q)
					return tt.tty == ttyYes, nil
				}
			}

			err := releaseWith(t, tt.pscli).Gate(context.Background(), opts)

			if code := exitcode.FromError(err); code != tt.wantCode {
				t.Fatalf("exit code = %d (%v), want %d", code, err, tt.wantCode)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if got := len(u.got) > 0; got != tt.wantUpdate {
				t.Errorf("self-updated = %v (%q), want %v", got, u.got, tt.wantUpdate)
			}
			if tt.wantUpdate && u.got[0] != tt.pscli {
				t.Errorf("self-updated to %q, want %q", u.got[0], tt.pscli)
			}
			if tt.selfUpdate && len(prompts) > 0 {
				t.Errorf("prompted %q despite --self-update", prompts)
			}
			warnings := warningTexts(rec.Events())
			if tt.wantWarn == "" && len(warnings) > 0 {
				t.Errorf("warnings = %q, want none", warnings)
			}
			if tt.wantWarn != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tt.wantWarn)) {
				t.Errorf("warnings = %q, want one containing %q", warnings, tt.wantWarn)
			}
		})
	}
}

func TestGatePassesOnSelfUpdateFailure(t *testing.T) {
	refused := exitcode.Precondition("pic-sure was installed by Homebrew; run brew upgrade pic-sure")
	u := &fakeUpdater{err: refused}
	err := releaseWith(t, "v2.1.0").Gate(context.Background(), release.GateOptions{
		CLIVersion: "v2.0.0", SelfUpdate: true, Updater: u,
	})
	if !errors.Is(err, refused) {
		t.Errorf("err = %v, want the updater's", err)
	}
}

func TestGatePassesOnPromptFailure(t *testing.T) {
	boom := errors.New("terminal closed")
	err := releaseWith(t, "v2.1.0").Gate(context.Background(), release.GateOptions{
		CLIVersion: "v2.0.0", Updater: &fakeUpdater{},
		Confirm: func(context.Context, string) (bool, error) { return false, boom },
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the prompt's", err)
	}
}

func warningTexts(evs []events.Event) []string {
	var out []string
	for _, e := range evs {
		if w, ok := e.(events.Warning); ok {
			out = append(out, w.Text)
		}
	}
	return out
}
