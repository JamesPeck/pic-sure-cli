package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newConfigCmd(a *App) *cobra.Command {
	return newGroup("config", "Show and change pic-sure.yaml",
		&cobra.Command{
			Use:   "show",
			Short: "Print the stack's config, defaults included",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.configShow(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:   "get KEY",
			Short: "Print one config value or section",
			Long: `Print one config value, such as network.http_port, or a whole section,
such as network. Secrets aren't config; they live in .pic-sure/secrets.yaml.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.configGet(cmd.OutOrStdout(), args[0])
			},
		},
		newConfigSetCmd(a),
		&cobra.Command{
			Use:   "edit",
			Short: "Edit the config in $EDITOR, validating on save",
			Long: `Open a copy of pic-sure.yaml in $VISUAL or $EDITOR (vi if neither is set).
When the editor exits, the copy is validated and saved over pic-sure.yaml.
If it's invalid, the editor reopens with the problems listed at the top;
exit without saving to give up. Needs a terminal.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.configEdit(cmd)
			},
		},
	)
}

func newConfigSetCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Validate and set one config value",
		Long: `Set one config value and save pic-sure.yaml, keeping its comments. The
whole config is validated first, and nothing is written if it's invalid.
A list takes comma-separated values or [a, b]; an empty VALUE clears it.
Flags go before KEY, so a VALUE such as -Xmx4g needs no quoting.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.configSet(cmd, args[0], args[1])
		},
	}
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func (a *App) configShow(w io.Writer) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if a.Global.JSON {
		return json.NewEncoder(w).Encode(cfg)
	}
	return writeYAML(w, cfg)
}

func (a *App) configGet(w io.Writer, key string) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	v, err := cfg.Get(key)
	if err != nil {
		return withUsageHint(configError(err))
	}
	if a.Global.JSON {
		return json.NewEncoder(w).Encode(map[string]any{"key": key, "value": v})
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.String, reflect.Int, reflect.Bool:
		_, err = fmt.Fprintln(w, v)
		return err
	default:
		return writeYAML(w, v)
	}
}

func (a *App) loadConfig() (*stack.Config, error) {
	st, err := a.openStack()
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, configError(err)
	}
	return cfg, nil
}

func (a *App) configSet(cmd *cobra.Command, key, value string) error {
	st, err := a.openStack()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	sink := a.newSink()
	lock, err := a.lockStack(cmd.Context(), cmd, st, sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	doc, err := st.ReadConfigDoc()
	if err != nil {
		return configError(err)
	}
	if err := doc.Set(key, value); err != nil {
		return withUsageHint(configError(err))
	}
	cfg, err := checkConfigDoc(doc, st.Dir)
	if err != nil {
		return configError(err)
	}
	data, err := doc.Bytes()
	if err != nil {
		return err
	}
	if err := st.WriteConfig(data); err != nil {
		return err
	}
	saved, _ := cfg.Get(key)
	return a.finish(map[string]any{"key": key, "value": saved}, nil)
}

// checkConfigDoc decodes and validates doc, including the files it names.
func checkConfigDoc(doc *stack.ConfigDoc, dir string) (*stack.Config, error) {
	cfg, err := doc.Config()
	if err != nil {
		return nil, err
	}
	if err := cfg.CheckFiles(dir); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (a *App) configEdit(cmd *cobra.Command) error {
	if a.Global.JSON || a.Global.NonInteractive || !a.IsTerminal() {
		return exitcode.Usage("config edit needs a terminal; use pic-sure config set KEY VALUE instead")
	}
	st, err := a.openStack()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	// Held while the editor is open, so no other command's change is lost
	// when the edit is saved over the file.
	lock, err := a.lockStack(cmd.Context(), cmd, st, a.newSink())
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	orig, err := st.ReadFile(stack.ConfigFile)
	if err != nil {
		return err
	}
	before, _ := stack.ParseConfigDoc(orig) // nil if it doesn't parse

	tmp, err := os.CreateTemp("", "pic-sure-*.yaml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Close(); err != nil {
		return err
	}

	content, header := orig, []byte(nil)
	var problem error
	for {
		if err := os.WriteFile(tmp.Name(), append(header, content...), 0o600); err != nil {
			return err
		}
		if err := a.runEditor(cmd.Context(), tmp.Name()); err != nil {
			return err
		}
		edited, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		edited = bytes.TrimPrefix(edited, header)
		if bytes.Equal(edited, content) {
			if problem != nil {
				return exitcode.Usage("%w\n%s is unchanged", problem, stack.ConfigFile)
			}
			_, err := fmt.Fprintf(a.Stderr, "%s is unchanged\n", stack.ConfigFile)
			return err
		}
		content = edited

		doc, err := stack.ParseConfigDoc(content)
		if err == nil && before != nil {
			err = doc.ReadOnlyChanges(before)
		}
		if err == nil {
			_, err = checkConfigDoc(doc, st.Dir)
		}
		if err == nil {
			return st.WriteConfig(content)
		}
		if !isConfigProblem(err) {
			return err
		}
		problem, header = err, editHeader(err)
	}
}

// editHeader lists err's problems as comments to put above the config in
// the editor, with line numbers shifted past the header itself.
func editHeader(err error) []byte {
	var problems []stack.Problem
	var ce *stack.ConfigError
	if errors.As(err, &ce) {
		problems = ce.Problems
	} else {
		problems = []stack.Problem{{Msg: err.Error()}}
	}
	shift := len(problems) + 3
	var b strings.Builder
	b.WriteString("# pic-sure: this config is invalid, so it wasn't saved:\n")
	for _, p := range problems {
		if p.Line > 0 {
			p.Line += shift
		}
		b.WriteString("#   " + p.String() + "\n")
	}
	b.WriteString("# Fix it and save, or exit without saving to give up.\n#\n")
	return []byte(b.String())
}

// runEditor opens path in $VISUAL, $EDITOR or vi. The editor variable may
// hold arguments, as in "code --wait", so the shell runs it.
func (a *App) runEditor(ctx context.Context, path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	if err := cmd.Run(); err != nil {
		return exitcode.Failed("editor %q failed (%v); %s is unchanged", editor, err, stack.ConfigFile)
	}
	return nil
}

// configError gives a config problem its exit code: a bad key or value is
// a usage error, and a file in another schema is incompatible.
func configError(err error) error {
	var se *stack.SchemaVersionError
	switch {
	case errors.As(err, &se):
		return exitcode.Incompatible("%w", err)
	case isConfigProblem(err):
		return exitcode.Usage("%w", err)
	}
	return err
}

// isConfigProblem reports whether err is about the config's content: a bad
// key, a bad value or another schema.
func isConfigProblem(err error) bool {
	var ce *stack.ConfigError
	var ke *stack.KeyError
	var se *stack.SchemaVersionError
	return errors.As(err, &ce) || errors.As(err, &ke) || errors.As(err, &se)
}

func writeYAML(w io.Writer, v any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return enc.Close()
}
