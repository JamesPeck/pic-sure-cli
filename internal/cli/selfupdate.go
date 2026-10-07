package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/selfupdate"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// releaseAPIEnv points self-update at another GitHub API root, such as a
// mirror or a test server.
const releaseAPIEnv = "PIC_SURE_RELEASE_API"

func newSelfUpdateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this binary with a verified release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			to, err := cmd.Flags().GetString("to")
			if err != nil {
				return err
			}
			sink := a.newSink()
			u := a.newSelfUpdater(a.selfUpdateProxy(sink), sink, "self-update")
			res, err := u.Install(cmd.Context(), to)
			if err != nil {
				return err
			}
			return a.finish(res, func(w io.Writer) error { return writeSelfUpdate(w, res) })
		},
	}
	c.Flags().String("to", "", "install this `VERSION` instead of the latest")
	return c
}

var _ release.SelfUpdater = (*selfupdate.Updater)(nil)

// newSelfUpdater returns the Updater for this run: the self-update command's,
// and the compatibility gate's (release.GateOptions.Updater) in init and
// update, which pass their stack's proxy.ProxyURL.
func (a *App) newSelfUpdater(proxy func(*http.Request) (*url.URL, error), sink events.Sink, step string) *selfupdate.Updater {
	return &selfupdate.Updater{
		Current:      a.Info.Version,
		APIBase:      os.Getenv(releaseAPIEnv),
		Proxy:        proxy,
		VerifyBundle: selfupdate.CosignVerifier(a.newRunner(a.newLogger()), selfupdate.DefaultRepo, exec.LookPath),
		Sink:         sink,
		Step:         step,
	}
}

// selfUpdateProxy is the proxy of the stack self-update runs in, if any,
// else the environment's (HTTPS_PROXY and so on). A stack whose config
// doesn't load falls back to the environment with a warning: an old
// pic-sure may need updating precisely because it can't read the config.
func (a *App) selfUpdateProxy(sink events.Sink) func(*http.Request) (*url.URL, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return http.ProxyFromEnvironment
	}
	dir, err := stack.Find(a.Global.Stack, cwd)
	if err != nil {
		return http.ProxyFromEnvironment
	}
	p, err := stackProxy(dir)
	if err != nil {
		sink.Emit(events.Warning{ID: "self-update", Text: fmt.Sprintf(
			"using the environment's proxy settings, not the stack's: %v", err)})
		return http.ProxyFromEnvironment
	}
	if p == nil {
		return http.ProxyFromEnvironment
	}
	return p.ProxyURL
}

// stackProxy resolves the proxy block of the stack in dir; nil when it sets
// none.
func stackProxy(dir string) (*netproxy.Proxy, error) {
	st, err := stack.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Proxy.HTTP == "" && cfg.Proxy.HTTPS == "" {
		return nil, nil
	}
	return netproxy.New(netproxy.Config{HTTP: cfg.Proxy.HTTP, HTTPS: cfg.Proxy.HTTPS, NoProxy: cfg.Proxy.NoProxy}, netproxy.CatalogServices())
}

// writeSelfUpdate is the human summary.
func writeSelfUpdate(w io.Writer, r *selfupdate.Result) error {
	var err error
	switch {
	case r.Updated:
		_, err = fmt.Fprintf(w, "Updated pic-sure from %s to %s (%s)\n", r.From, r.To, r.Path)
	case r.To == r.From:
		_, err = fmt.Fprintf(w, "pic-sure is already %s\n", r.To)
	default:
		_, err = fmt.Fprintf(w, "pic-sure %s is newer than the latest release %s; nothing to do\n", r.From, r.To)
	}
	return err
}
