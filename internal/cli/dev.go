package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func newDevCmd(a *App) *cobra.Command {
	return newGroup("dev", "Run services from local source with debug ports",
		&cobra.Command{
			Use:   "list",
			Short: "List the services that have a dev mode",
			Args:  cobra.NoArgs,
			RunE:  a.devList,
		},
		&cobra.Command{
			Use:   "on SERVICE",
			Short: "Run SERVICE from local source",
			Long: `Build SERVICE's component from its local checkout
(components.<component>.source), add SERVICE to dev.services, re-render and
recreate SERVICE. psama, hpds, gateway, operations and query also get a
JDWP debug port on 127.0.0.1, from the stack's network.dev_ports block;
dev list shows it. On a stopped stack, pic-sure up starts it.

Run it again after changing the source to rebuild and recreate.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return a.dev(cmd, args[0], true) },
		},
		&cobra.Command{
			Use:   "off SERVICE",
			Short: "Remove SERVICE's dev variant and debug port",
			Long: `Remove SERVICE from dev.services, re-render and recreate SERVICE without
its debug port. While components.<component>.source is set, SERVICE keeps
running the build of that checkout, since a source applies to the whole
component. To return to the release images, unset the source and run
pic-sure up.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return a.dev(cmd, args[0], false) },
		},
	)
}

// devListReport is `dev list --json`'s data.
type devListReport struct {
	Variants []ops.DevVariantInfo `json:"variants"`
}

func (a *App) devList(cmd *cobra.Command, _ []string) error {
	cfg, doc, err := a.readConfig(cmd, "dev.services, network.dev_ports.base and the components' sources are read as written, and the rest is defaults")
	if err != nil {
		return err
	}
	var variants []ops.DevVariantInfo
	if cfg != nil {
		variants = ops.DevList(cfg)
	} else {
		variants = devListAsWritten(doc)
	}
	report := devListReport{Variants: variants}
	return a.printReport(report, func(w io.Writer) error { return writeDevList(w, report.Variants) })
}

// devListAsWritten is dev list for a pic-sure.yaml whose schema this
// pic-sure can't decode: the default config's list, with each variant's
// state, port and source taken from the keys that set them, where they
// hold what this pic-sure would expect there.
func devListAsWritten(doc *stack.ConfigDoc) []ops.DevVariantInfo {
	def := stack.DefaultConfig()
	vs := ops.DevList(&def)
	var on []any
	if v, err := doc.Raw("dev.services"); err == nil {
		on, _ = v.([]any)
	}
	shift := 0
	if v, err := doc.Raw("network.dev_ports.base"); err == nil {
		if base, ok := v.(int); ok {
			shift = base - def.Network.DevPorts.Base
		}
	}
	for i := range vs {
		vs[i].On = slices.Contains(on, any(vs[i].Name))
		if vs[i].Port != 0 {
			vs[i].Port += shift
		}
		if v, err := doc.Raw("components." + vs[i].Component + ".source"); err == nil {
			vs[i].Source, _ = v.(string)
		}
	}
	return vs
}

func writeDevList(w io.Writer, vs []ops.DevVariantInfo) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SERVICE\tSTATE\tPORT\tCOMPONENT\tSOURCE")
	for _, v := range vs {
		state, port, src := "off", "-", "(not set)"
		if v.On {
			state = "on"
		}
		if v.Port != 0 {
			port = fmt.Sprintf("127.0.0.1:%d", v.Port)
		}
		if v.Source != "" {
			src = v.Source
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, state, port, v.Component, src)
	}
	return tw.Flush()
}

// devReport is `dev on|off --json`'s data.
type devReport struct {
	Service  string   `json:"service"`
	On       bool     `json:"on"`
	Services []string `json:"services"`
	// Port is the debug (or HMR) port on 127.0.0.1 while on, else 0.
	Port   int    `json:"port"`
	Source string `json:"source"`
	// built says the services run images built from Source, which they
	// keep after dev off.
	built bool
}

// dev is `dev on NAME` and `dev off NAME`.
func (a *App) dev(cmd *cobra.Command, name string, on bool) (err error) {
	v, err := ops.LookupDev(name)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d := a.newDeps()
	lock, err := a.lockStack(ctx, cmd, st, d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	doc, err := st.ReadConfigDoc()
	if err != nil {
		return configError(err)
	}
	cfg, err := checkConfigDoc(doc, st.Dir)
	if err != nil {
		return configError(err)
	}
	log.RegisterSecrets(cfg.Auth.AdminEmail)
	report := &devReport{Service: v.Name, On: on, Services: v.Services, Source: ops.ComponentSource(cfg, v.Component)}
	if !on && !slices.Contains(cfg.Dev.Services, v.Name) {
		return a.finish(report, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "dev %s is already off.\n", v.Name)
			return err
		})
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	if on {
		if err := ops.CheckDevOn(st.Dir, cfg, v); err != nil {
			return err
		}
	}
	sec, err := upSecrets(d, st, cfg)
	if err != nil {
		return err
	}
	if err := checkDevStack(cmd, d, st, cfg, v, on); err != nil {
		return err
	}
	proxy, err := netproxy.New(netproxy.Config(cfg.Proxy), netproxy.CatalogServices())
	if err != nil {
		return exitcode.Usage("%w", err)
	}
	d.Git = d.Git.WithEnv(proxy.Env()...)
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Open(root, cache.Options{Git: d.Git, Holder: cmd.CommandPath()})
	if err != nil {
		return err
	}
	if err := registerStack(ctx, c, d.Sink, st, cfg.Name); err != nil {
		return err
	}

	op := "dev off"
	if on {
		op = "dev on"
	}
	state.StartOperation(op, d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	plan, err := ops.DevSteps(d, st, doc, cfg, state, ops.DevOptions{
		ConvergeOptions: ops.ConvergeOptions{
			Cache:      c,
			CLIVersion: a.Info.Version,
			Compose:    a.upCompose(d, st, cfg, sec),
		},
		Variant: v.Name,
		On:      on,
	})
	if err == nil {
		err = steps.Run(ctx, d.Sink, plan, steps.Options{})
	}
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	if on {
		report.Port = ops.DevPort(cfg, v)
	}
	report.built = state.Components[v.Component].Source != ""
	return a.finish(report, func(w io.Writer) error { return writeDev(w, report, v) })
}

func writeDev(w io.Writer, r *devReport, v catalog.DevVariant) error {
	if r.On {
		_, err := fmt.Fprintf(w, "dev %s is on: %s %s from %s", r.Service, strings.Join(r.Services, ", "), verb(r.Services, "runs", "run"), r.Source)
		if err == nil && r.Port != 0 {
			if v.Image != "" {
				_, err = fmt.Fprintf(w, "; browse to %s", render.HMROrigin(r.Port))
			} else {
				_, err = fmt.Fprintf(w, "; attach a debugger (JDWP) to 127.0.0.1:%d", r.Port)
			}
		}
		if err == nil {
			_, err = fmt.Fprintln(w, ".")
		}
		return err
	}
	var err error
	if v.Image != "" {
		_, err = fmt.Fprintf(w, "dev %s is off: %s no longer %s the %s dev server.\n", r.Service, strings.Join(r.Services, ", "), verb(r.Services, "runs", "run"), v.Image)
	} else {
		_, err = fmt.Fprintf(w, "dev %s is off: %s %s no debug port.\n", r.Service, strings.Join(r.Services, ", "), verb(r.Services, "has", "have"))
	}
	if err == nil && r.built {
		_, err = fmt.Fprintf(w, "%s still %s the build of components.%s.source (%s), which applies to the whole component.\n"+
			"To return to the release images: pic-sure config set components.%s.source '' && pic-sure up\n",
			strings.Join(r.Services, ", "), verb(r.Services, "runs", "run"), v.Component, r.Source, v.Component)
	}
	return err
}

// verb is one or many by the number of services.
func verb(services []string, one, many string) string {
	if len(services) == 1 {
		return one
	}
	return many
}

// checkDevStack makes the ownership check and, for dev on, makes sure the
// port v publishes is free or already published by the stack's own
// containers, so either is exit 3 before anything is built or recreated.
func checkDevStack(cmd *cobra.Command, d *ops.Deps, st *stack.Stack, cfg *stack.Config, v catalog.DevVariant, on bool) error {
	owned, err := ops.CheckOwnership(cmd.Context(), d, st, cfg.Name)
	if err != nil {
		return err
	}
	if port := ops.DevPort(cfg, v); on && port != 0 && !owned.Published[port] && !(systemHost{}).PortFree(port) {
		return exitcode.Precondition("port %d, dev %s's port from network.dev_ports.base (%d), is in use", port, v.Name, cfg.Network.DevPorts.Base)
	}
	return nil
}
