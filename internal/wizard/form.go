package wizard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// summaryDimStyle renders the "(default)" marker faintly, as an annotation
// rather than part of the value.
var summaryDimStyle = lipgloss.NewStyle().Faint(true)

const (
	clientSecretKey = "auth.auth0.client_secret"
	rootPasswordKey = "db.remote.root_password"
	proxyHTTPKey    = "proxy.http"
	proxyHTTPSKey   = "proxy.https"
)

// Form is the setup form: Main asks for the fields, and Confirm, built by
// BuildConfirm once Main completes, summarizes them and asks to go ahead.
type Form struct {
	// base is the config the form opened with. It holds every field the
	// form doesn't ask for.
	base stack.Config
	// vals are the values entered, bound to the huh fields; seed is what
	// they opened with, and def the defaults the summary marks.
	vals map[string]*string
	seed map[string]string
	def  map[string]string

	useProxy, seedProxy bool
	// httpsInput, httpsFollows and httpsSynced pre-fill the HTTPS proxy
	// from the HTTP one (spec §9.10) until the user edits it, if the two
	// were equal when the form opened.
	httpsInput   *huh.Input
	httpsFollows bool
	httpsSynced  string

	// secrets seeds the secret fields.
	secrets stack.UserSecrets

	confirmed bool

	Main    *huh.Form
	Confirm *huh.Form
}

// NewForm returns the form, opened with base's values and the secrets in
// sec. base is also the defaults the summary marks.
func NewForm(base stack.Config, sec stack.UserSecrets) *Form {
	return NewFormFrom(base, base, sec)
}

// NewFormFrom returns the form opened with base's values (the defaults
// themselves, or a failed setup's answers) and the secrets in sec. The
// summary marks a value "(default)" only when it matches defaults.
func NewFormFrom(defaults, base stack.Config, sec stack.UserSecrets) *Form {
	f := &Form{
		base:     base,
		secrets:  sec,
		vals:     map[string]*string{},
		seed:     map[string]string{},
		def:      map[string]string{},
		useProxy: base.Proxy.HTTP != "" || base.Proxy.HTTPS != "",
	}
	f.seedProxy = f.useProxy
	var groups []*huh.Group
	for _, g := range Groups {
		var fields []huh.Field
		if g.askProxy {
			fields = append(fields, huh.NewSelect[bool]().
				Options(huh.NewOption("No proxy", false), huh.NewOption("Use a proxy", true)).
				Value(&f.useProxy))
		}
		for _, it := range g.Items {
			fields = append(fields, f.field(it, defaults))
		}
		hg := huh.NewGroup(fields...).Title(g.Title).Description(g.Description)
		if g.Shown != nil {
			shown := g.Shown
			hg = hg.WithHideFunc(func() bool { return !shown(f) })
		}
		groups = append(groups, hg)
	}
	f.httpsSynced = f.Value(proxyHTTPSKey)
	f.httpsFollows = f.httpsSynced == f.Value(proxyHTTPKey)
	f.Main = huh.NewForm(groups...)
	return f
}

// field builds the huh field for one item, bound to its value, and records
// its default from defaults.
func (f *Form) field(it Item, defaults stack.Config) huh.Field {
	sf, ok := stack.LookupField(it.Key)
	if !ok {
		panic("wizard: no config field " + it.Key)
	}
	v := ""
	switch {
	case it.Key == clientSecretKey:
		v = string(f.secrets.Auth0ClientSecret)
	case it.Key == rootPasswordKey:
		v = string(f.secrets.DBRemoteRootPassword)
	case !sf.Secret:
		if got, err := f.base.Get(it.Key); err == nil {
			v = fmt.Sprint(got)
		}
	}
	f.vals[it.Key], f.seed[it.Key] = &v, v
	if got, err := defaults.Get(it.Key); err == nil && !sf.Secret {
		f.def[it.Key] = fmt.Sprint(got)
	}
	if len(sf.Options) > 0 {
		opts := make([]huh.Option[string], len(sf.Options))
		for i, o := range sf.Options {
			opts[i] = huh.NewOption(o, o)
		}
		return huh.NewSelect[string]().Title(it.Title).Description(sf.Help).Options(opts...).Value(&v)
	}
	// init's flags read secrets from stdin; the form doesn't.
	help := strings.TrimSuffix(sf.Help, " Read from stdin.")
	in := huh.NewInput().Title(it.Title).Description(help).Value(&v).Validate(f.validator(it.Key))
	if sf.Secret {
		in = in.EchoMode(huh.EchoModePassword)
	}
	if it.Key == proxyHTTPSKey {
		f.httpsInput = in
	}
	return in
}

// Value is the value entered for key so far.
func (f *Form) Value(key string) string {
	if p := f.vals[key]; p != nil {
		return *p
	}
	return ""
}

// Update passes msg to Main, then pre-fills the HTTPS proxy
// (syncHTTPSProxy).
func (f *Form) Update(msg tea.Msg) tea.Cmd {
	m, cmd := f.Main.Update(msg)
	if mf, ok := m.(*huh.Form); ok {
		f.Main = mf
	}
	f.syncHTTPSProxy()
	return cmd
}

// syncHTTPSProxy copies the HTTP proxy into the HTTPS one while the HTTPS
// one still holds the last value copied, so clearing or editing it sticks.
// A form that opened with the two different (a reopened setup whose HTTPS
// proxy was cleared) never copies.
func (f *Form) syncHTTPSProxy() {
	http, https := f.vals[proxyHTTPKey], f.vals[proxyHTTPSKey]
	if *https != f.httpsSynced {
		f.httpsFollows = false
	}
	if !f.httpsFollows || *https == *http {
		return
	}
	*https = *http
	f.httpsSynced = *http
	f.httpsInput.Value(https) // huh shows the bound value only when it is set
}

// Dirty reports whether anything differs from what the form opened with.
func (f *Form) Dirty() bool {
	if f.useProxy != f.seedProxy {
		return true
	}
	for k, p := range f.vals {
		if *p != f.seed[k] {
			return true
		}
	}
	return false
}

// shown reports whether g applies to the values entered so far.
func (f *Form) shown(g Group) bool { return g.Shown == nil || g.Shown(f) }

// Result is the config and secrets entered. Fields of groups that don't
// apply keep the form's opening values, except the proxy, which is
// cleared when the user said there is none.
func (f *Form) Result() (*stack.ConfigDoc, stack.UserSecrets, error) {
	return f.result("", "")
}

// result is Result with key's value replaced by v (for validating a
// value before huh stores it), when key isn't empty.
func (f *Form) result(key, v string) (*stack.ConfigDoc, stack.UserSecrets, error) {
	val := func(k string) string {
		if k == key {
			return v
		}
		return f.Value(k)
	}
	base := f.base
	base.Name = val("name") // read-only, so Set refuses it
	doc, err := stack.NewConfigDoc(&base)
	if err != nil {
		return nil, stack.UserSecrets{}, err
	}
	var sec stack.UserSecrets
	for _, g := range Groups {
		applies := f.shown(g)
		for _, it := range g.Items {
			x := val(it.Key)
			switch it.Key {
			case "name":
			case clientSecretKey:
				if applies {
					sec.Auth0ClientSecret = stack.Secret(x)
				}
			case rootPasswordKey:
				if applies {
					sec.DBRemoteRootPassword = stack.Secret(x)
				}
			default:
				if !applies {
					if !strings.HasPrefix(it.Key, "proxy.") {
						continue
					}
					x = "" // the user said there is no proxy
				}
				if err := doc.Set(it.Key, x); err != nil {
					return nil, sec, err
				}
			}
		}
	}
	if err := moveDevPorts(doc, f.base.Network.DevPorts.Base, val("network.http_port"), val("network.https_port")); err != nil {
		return nil, sec, err
	}
	return doc, sec, nil
}

// moveDevPorts moves the dev-port block, which the form doesn't ask for,
// out of the way of the ports entered, in init's steps of 10. init chooses
// a free block on the host again anyway.
func moveDevPorts(doc *stack.ConfigDoc, base int, ports ...string) error {
	moved := base
	for clash := true; clash; {
		clash = false
		for _, p := range ports {
			if n, err := strconv.Atoi(p); err == nil && n >= moved && n < moved+stack.DevPortCount {
				clash, moved = true, moved+10
			}
		}
	}
	if moved == base {
		return nil
	}
	return doc.SetValue("network.dev_ports.base", moved)
}

// validator checks key's value in the context of everything else entered:
// the config's validation problems at key, or a missing secret.
func (f *Form) validator(key string) func(string) error {
	return func(s string) error {
		doc, sec, err := f.result(key, s)
		if err != nil {
			return problemAt(err, key)
		}
		if key == clientSecretKey || key == rootPasswordKey {
			return f.checkSecret(doc, sec, key)
		}
		_, err = doc.Config()
		return problemAt(err, key)
	}
}

// checkSecret refuses a secret the config requires and doesn't have, or a
// client secret PSAMA would refuse.
func (f *Form) checkSecret(doc *stack.ConfigDoc, sec stack.UserSecrets, key string) error {
	v := sec.Auth0ClientSecret
	if key == rootPasswordKey {
		v = sec.DBRemoteRootPassword
	}
	sf, _ := stack.LookupField(key)
	cfg, err := doc.Config()
	var ce *stack.ConfigError
	if err != nil && !errors.As(err, &ce) {
		return err
	}
	if cfg == nil {
		// Some other field is invalid; judge with the modes entered.
		c := f.base
		c.Auth.Mode = stack.AuthMode(f.Value("auth.mode"))
		c.DB.Mode = stack.DBMode(f.Value("db.mode"))
		cfg = &c
	}
	switch {
	case v == "" && sf.Required(cfg):
		return fmt.Errorf("required when %s", sf.RequiredWhen.Desc)
	case key == clientSecretKey && v != "" && len(v) < jwt.MinSecretLen:
		return fmt.Errorf("%d bytes; PSAMA needs at least %d", len(v), jwt.MinSecretLen)
	}
	return nil
}

// problemAt is err's problem at key, if it has one. A problem at another
// field the form asks for is that field's to report; one at a key the
// form doesn't ask for is reported here, so it can't surface only on the
// confirm page.
func problemAt(err error, key string) error {
	var ce *stack.ConfigError
	if !errors.As(err, &ce) {
		return err
	}
	for _, p := range ce.Problems {
		if p.Path == key {
			return errors.New(p.Msg)
		}
	}
	for _, p := range ce.Problems {
		if !asked(p.Path) {
			return fmt.Errorf("%s: %s", p.Path, p.Msg)
		}
	}
	return nil
}

// asked reports whether the form asks for key.
func asked(key string) bool { return item(key).Key != "" }

// item is the form's item for key, or the zero Item.
func item(key string) Item {
	for _, g := range Groups {
		for _, it := range g.Items {
			if it.Key == key {
				return it
			}
		}
	}
	return Item{}
}

// Check validates everything entered, as init will.
func (f *Form) Check() error {
	doc, sec, err := f.Result()
	if err != nil {
		return err
	}
	if _, err := doc.Config(); err != nil {
		return err
	}
	for _, key := range []string{clientSecretKey, rootPasswordKey} {
		if err := f.checkSecret(doc, sec, key); err != nil {
			return fmt.Errorf("%s: %w", item(key).Title, err)
		}
	}
	return nil
}

// BuildConfirm builds Confirm: a summary of what was entered and a yes/no.
// Yes is refused while anything is invalid. Call it once Main completes.
func (f *Form) BuildConfirm() *huh.Form {
	f.confirmed = false
	confirm := huh.NewConfirm().
		Title("Create the stack with these settings?").
		Description(f.summary()).
		Affirmative("Create").
		Negative("Cancel").
		Value(&f.confirmed).
		Validate(func(yes bool) error {
			if !yes {
				return nil
			}
			return f.Check()
		})
	f.Confirm = huh.NewForm(huh.NewGroup(confirm))
	return f.Confirm
}

// Confirmed reports whether the user said yes on Confirm.
func (f *Form) Confirmed() bool { return f.confirmed }

// summary lists the fields that apply, aligned, with secrets masked, empty
// optional fields left out and "(default)" after a value equal to its
// default.
func (f *Form) summary() string {
	type row struct{ title, value, note string }
	var rows []row
	for _, g := range Groups {
		if !f.shown(g) {
			continue
		}
		if g.askProxy && !f.useProxy {
			rows = append(rows, row{title: "Proxy", value: "none"})
		}
		for _, it := range g.Items {
			v := f.Value(it.Key)
			sf, _ := stack.LookupField(it.Key)
			switch {
			case v == "":
				continue
			case sf.Secret:
				v = "********"
			}
			r := row{title: it.Title, value: v}
			if d, ok := f.def[it.Key]; ok && v == d {
				r.note = " " + summaryDimStyle.Render("(default)")
			}
			rows = append(rows, r)
		}
	}
	width := 0
	for _, r := range rows {
		width = max(width, lipgloss.Width(r.title))
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.title + strings.Repeat(" ", width-lipgloss.Width(r.title)) + "  " + r.value + r.note + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
