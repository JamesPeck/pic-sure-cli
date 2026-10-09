package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

// demoReport is data demo's --json data.
type demoReport struct {
	// Dataset is the provenance marker the load wrote: demo:<name>.
	Dataset string `json:"dataset"`
}

func (a *App) dataDemo(cmd *cobra.Command, args []string) error {
	name := "nhanes"
	if len(args) == 1 {
		name = args[0]
	}
	heap, _ := cmd.Flags().GetInt("heap")
	if heap < 0 || cmd.Flags().Changed("heap") && heap == 0 {
		return exitcode.Usage("--heap must be a positive number of MB, not %d", heap)
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

	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	if err := ops.RefuseSharedHPDS(cfg); err != nil {
		return err
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	compose, cfg, sec, err := a.stackComposeConfig(cmd, d.Runner, st)
	if err != nil {
		return err
	}
	d.Compose = compose
	proxy, err := netproxy.New(netproxy.Config(cfg.Proxy), netproxy.CatalogServices())
	if err != nil {
		return configError(err)
	}
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Open(root, cache.Options{Git: d.Git, Holder: cmd.CommandPath()})
	if err != nil {
		return err
	}

	if err := checkOwned(cmd, d, st, cfg); err != nil {
		return err
	}
	state.StartOperation("data demo", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	dataset, err := ops.DataDemo(ctx, d, st, cfg, sec, state, ops.DemoOptions{
		Dataset: name,
		HeapMB:  heap,
		Cache:   c,
		HTTP:    demoHTTPClient(proxy),
	})
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	return a.finish(demoReport{Dataset: dataset}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Loaded the %s demo data into HPDS and the dictionary; HPDS and dictionary-api are healthy.\n", name)
		return err
	})
}

// demoHTTPClient downloads through the stack's proxy, or the environment's
// when the stack sets none, as self-update does.
func demoHTTPClient(proxy *netproxy.Proxy) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if proxy.Enabled() {
		tr.Proxy = proxy.ProxyURL
	}
	return &http.Client{Transport: tr}
}
