package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

type upPlan struct {
	Stack       string              `json:"stack"`
	DryRun      bool                `json:"dry_run"`
	Partial     bool                `json:"partial"`
	Notice      string              `json:"notice"`
	Preparation []upPreparation     `json:"preparation"`
	Probes      []upProbe           `json:"probes"`
	Steps       []steps.PlannedStep `json:"steps"`
}

type upPreparation struct {
	ID         string `json:"id"`
	WouldWrite bool   `json:"would_write"`
	Reason     string `json:"reason"`
}

type upProbe struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

func (a *App) upDryRun(cmd *cobra.Command) error {
	st, err := a.openStackUnlogged(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	log.RegisterSecrets(cfg.Auth.AdminEmail)
	if err := checkSkipSteps(cmd, ops.UpStepIDs(cfg), a.Global.SkipSteps); err != nil {
		return err
	}
	if err := cfg.CheckFiles(st.Dir); err != nil {
		return configError(err)
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	sec, err := st.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		return exitcode.Precondition("the stack in %s has no %s; run `pic-sure init %s` to finish it", st.Dir, stack.SecretsFile, st.Dir)
	}
	if err != nil {
		return err
	}
	if err := refuseClientSecret(cfg, sec); err != nil {
		return err
	}
	if sec.Auth0ClientSecret != "" && len(sec.Auth0ClientSecret) < jwt.MinSecretLen {
		return shortClientSecret()
	}
	if cfg.DB.Mode == stack.DBRemote && sec.DBRemoteRootPassword == "" {
		return exitcode.Precondition("the stack uses a remote database, but no root password was given for it")
	}
	if err := st.RequireHPDSKey(); err != nil {
		return err
	}
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Inspect(root)
	if err != nil {
		return err
	}

	needsSecrets := sec.NeedsGeneration(cfg.Auth.Mode == stack.AuthOpen)
	report := &upPlan{
		Stack: cfg.Name, DryRun: true, Probes: []upProbe{},
		Notice: "Partial preview of current inputs, without a stack lock. Preparation writes are skipped; generated secrets and a backfilled stack ID are not simulated. Checks stop at the first required action. Concurrent changes can invalidate this snapshot.",
		Preparation: []upPreparation{
			{ID: "stack-lock", WouldWrite: true, Reason: "would acquire the stack lock; skipped"},
			{ID: "stack-id", WouldWrite: state.StackID == "", Reason: "would backfill a missing stack ID; skipped"},
			{ID: "secrets", WouldWrite: needsSecrets, Reason: "would generate missing secrets; skipped; Checks use existing inputs"},
			{ID: "cache", WouldWrite: true, Reason: "would ensure cache directories, take the cache use lock and register the stack; skipped"},
			{ID: "state", WouldWrite: true, Reason: "would record operation start and completion in state.json; skipped"},
			{ID: "run-log", WouldWrite: true, Reason: "would open the run log; skipped even at debug level"},
		},
	}
	d := a.newDeps()
	d.Docker = &upProbeEngine{Engine: d.Docker, report: report, sink: d.Sink}
	blocked := map[string]string{}
	if needsSecrets {
		for _, id := range []string{ops.StepDB, ops.StepDBBootstrap, ops.StepMigrate, ops.StepSeed} {
			blocked[id] = "not checked: depends on skipped secret generation"
		}
	}
	if cfg.DB.Mode == stack.DBRemote {
		for _, id := range []string{ops.StepDB, ops.StepDBBootstrap, ops.StepMigrate, ops.StepSeed} {
			blocked[id] = "not checked: remote database Check requires a client container with networking, which is outside this preview"
		}
	}
	list := ops.UpSteps(d, st, cfg, sec, state, ops.ConvergeOptions{Cache: c, CLIVersion: a.Info.Version, Compose: a.upCompose(d, st, cfg, sec)})
	report.Steps, err = steps.Plan(cmd.Context(), list, steps.PlanOptions{Skip: a.Global.SkipSteps, BlockedChecks: blocked})
	if err != nil {
		return err
	}
	for i := range report.Steps {
		p := &report.Steps[i]
		if p.ID == ops.GenomicLeftoversStepID && p.Status == steps.WouldAct {
			p.Reason = "interrupted genomic promote leftovers would block up; recover the data in its owning stack with pic-sure data load-genomic --recover"
		}
		if p.Status == steps.DependsOnEarlierStep {
			report.Partial = true
		}
	}
	return a.finish(report, func(w io.Writer) error {
		if _, err := fmt.Fprintln(w, report.Notice); err != nil {
			return err
		}
		for _, p := range report.Preparation {
			if _, err := fmt.Fprintf(w, "preparation %s: would_write=%t; %s\n", p.ID, p.WouldWrite, p.Reason); err != nil {
				return err
			}
		}
		for _, p := range report.Probes {
			if _, err := fmt.Fprintf(w, "Read-only probe container was run: %s (%s)\n", p.Name, p.Image); err != nil {
				return err
			}
		}
		for _, p := range report.Steps {
			if _, err := fmt.Fprintf(w, "%s: %s — %s\n", p.ID, p.Status, p.Reason); err != nil {
				return err
			}
		}
		return nil
	})
}

type upProbeEngine struct {
	docker.Engine
	report *upPlan
	sink   events.Sink
}

func (e *upProbeEngine) Run(ctx context.Context, opts docker.RunOpts) (int, error) {
	if !opts.Remove || opts.Network != "none" {
		return 0, errors.New("dry-run probe must self-remove and disable networking")
	}
	for _, m := range opts.Mounts {
		if !m.ReadOnly {
			return 0, errors.New("dry-run probe requires read-only mounts")
		}
	}
	// The reachable container Check is genomic-leftovers' directory listing.
	// Database client Checks requiring networking are blocked before Plan.
	opts.PullNever = true
	e.sink.Emit(events.Warning{ID: ops.GenomicLeftoversStepID, Text: "Running read-only probe container " + opts.Name + " (" + opts.Image + ")"})
	code, err := e.Engine.Run(ctx, opts)
	e.report.Probes = append(e.report.Probes, upProbe{Name: opts.Name, Image: opts.Image})
	if errors.Is(err, docker.ErrNotFound) {
		if present, inspectErr := e.ImageExists(ctx, opts.Image); inspectErr == nil && !present {
			return code, exitcode.Precondition("dry-run needs local probe image %s; pull it explicitly with `docker pull %s` and retry: %w", opts.Image, opts.Image, err)
		}
	}
	return code, err
}

func (e *upProbeEngine) Rm(ctx context.Context, name string, force bool) error {
	if !slices.ContainsFunc(e.report.Probes, func(p upProbe) bool { return p.Name == name }) {
		return nil
	}
	return e.Engine.Rm(ctx, name, force)
}
