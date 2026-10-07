package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// TruststoreStepID is the truststore step's ID, for --skip-step.
const TruststoreStepID = "truststore"

// truststoreVolume is the catalog volume psama's truststore lives in. The
// truststore fragment mounts it read-only at /truststore, and render points
// psama at /truststore/cacerts with the password "changeit".
const truststoreVolume = "truststore"

// truststoreScript runs in the helper container with sh. It copies the
// image's cacerts, imports the PEM certificates read from stdin one by one
// under the aliases given as arguments, in order, and then renames the
// result into place, so a failed run leaves the previous truststore intact.
// Only shell builtins, rm, cp, mv, chmod and keytool are needed.
const truststoreScript = `set -eu
tmp=/truststore/.cacerts.tmp
rm -f "$tmp"
cp "${JAVA_HOME:?JAVA_HOME is not set in the image}/lib/security/cacerts" "$tmp"
n=0
pem=
while IFS= read -r line; do
	pem="$pem$line
"
	[ "$line" = "-----END CERTIFICATE-----" ] || continue
	n=$((n + 1))
	eval "alias=\${$n}"
	if ! out=$(printf %s "$pem" | keytool -importcert -noprompt -keystore "$tmp" -storepass changeit -alias "$alias" 2>&1); then
		echo "$alias: $out" >&2
		exit 1
	fi
	echo "$alias: $out"
	pem=
done
if [ "$n" -ne "$#" ]; then
	echo "read $n certificates from stdin, expected $#" >&2
	exit 1
fi
chmod 0644 "$tmp"
mv -f "$tmp" /truststore/cacerts
`

var certExts = []string{".crt", ".pem", ".cer", ".der"}

// A CustomCert is one operator CA certificate from trust.custom_certs_dir.
type CustomCert struct {
	// File is the name of the file in the certs directory it came from.
	File string
	// Alias is its alias in the truststore: custom-<n>-<file name>, with n
	// counting the certificates from 1 and the name lower-cased and
	// stripped to [a-z0-9._-].
	Alias string
	DER   []byte
}

// CustomCerts returns the operator's CA certificates (§9.5): every
// certificate in the *.crt, *.pem, *.cer and *.der files of the config's
// trust.custom_certs_dir, in file name order. A relative directory is
// relative to the stack directory. A PEM file may hold several certificates;
// a file without a PEM header is read as one DER certificate. Hidden files
// are ignored, so are other files, and so is a missing directory. No
// certificates means the stack has no custom trust: render then leaves the
// truststore out (catalog.Mode.CustomTrust is false), and the truststore
// step does nothing.
func CustomCerts(st *stack.Stack, cfg *stack.Config) ([]CustomCert, error) {
	dir := cfg.Trust.CustomCertsDir
	if dir == "" {
		return nil, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(st.Dir, dir)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust.custom_certs_dir: %w", err)
	}
	var certs []CustomCert
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !slices.Contains(certExts, strings.ToLower(filepath.Ext(name))) {
			continue
		}
		file := filepath.Join(dir, name)
		fi, err := os.Stat(file) // a symlinked cert counts
		if err != nil {
			return nil, fmt.Errorf("trust.custom_certs_dir: %w", err)
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("trust.custom_certs_dir: %w", err)
		}
		ders, err := certDERs(data)
		if err != nil {
			return nil, fmt.Errorf("trust.custom_certs_dir: %s: %w", file, err)
		}
		for _, der := range ders {
			alias := fmt.Sprintf("custom-%d-%s", len(certs)+1, aliasName(name))
			certs = append(certs, CustomCert{File: name, Alias: alias, DER: der})
		}
	}
	return certs, nil
}

// certDERs returns the certificates in a cert file: each block of a PEM
// file, or the whole file if it isn't PEM. Every PEM block must decode and
// be a CERTIFICATE, so no certificate is dropped unnoticed. keytool checks
// that each one is a certificate.
func certDERs(data []byte) ([][]byte, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	begins := bytes.Count(data, []byte("-----BEGIN "))
	if begins == 0 {
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, errors.New("the file is empty")
		}
		return [][]byte{data}, nil
	}
	var ders [][]byte
	for rest := data; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a PEM %s block, not a CERTIFICATE", b.Type)
		}
		ders = append(ders, b.Bytes)
	}
	if len(ders) != begins {
		return nil, fmt.Errorf("%d of its %d PEM blocks don't decode", begins-len(ders), begins)
	}
	return ders, nil
}

// aliasName is a file name as it goes in an alias: lower-cased, since
// keytool lower-cases aliases, and stripped to [a-z0-9._-].
func aliasName(file string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return -1
	}, file)
}

// TruststoreStep returns the step that builds psama's truststore (§9.5,
// D16) when the stack has custom CA certs (CustomCerts), and does nothing
// otherwise. It runs a helper container from psamaImage, the psama image
// the stack runs, that copies the image's own cacerts (with the AWS
// certificate its Dockerfile imports) into the stack's truststore volume
// and imports each custom cert with the image's keytool. The certificates
// reach it on stdin, so their directory needn't be visible to the docker
// daemon, and a symlinked cert works. The volume is created first, with the
// labels compose expects, if compose hasn't made it yet.
//
// The step is done when state.json records a fill of the volume, as it is
// now, from the same certificates and image ID. A new psama image ID
// rebuilds the truststore, so the image's CA updates flow through. A running
// psama reads the new truststore only once restarted.
func TruststoreStep(d *Deps, st *stack.Stack, cfg *stack.Config, psamaImage string) steps.Step {
	t := &truststore{d: d, st: st, cfg: cfg, image: psamaImage}
	return steps.Step{
		ID:    TruststoreStepID,
		Title: "Build psama's truststore",
		Check: t.check,
		Apply: t.apply,
	}
}

type truststore struct {
	d     *Deps
	st    *stack.Stack
	cfg   *stack.Config
	image string
}

func (t *truststore) volume() string {
	v, _ := catalog.LookupVolume(truststoreVolume)
	return v.DockerName(t.cfg.Name)
}

// inputs returns the custom certs and the hash of everything the
// truststore is built from, or no certs and an empty hash when there are
// none.
func (t *truststore) inputs(ctx context.Context) ([]CustomCert, string, error) {
	certs, err := CustomCerts(t.st, t.cfg)
	if err != nil || len(certs) == 0 {
		return nil, "", err
	}
	id, err := t.d.Docker.ImageID(ctx, t.image)
	if err != nil {
		return nil, "", fmt.Errorf("the psama image %s: %w", t.image, err)
	}
	h := sha256.New()
	_, _ = io.WriteString(h, truststoreScript)
	for _, c := range certs {
		_, _ = fmt.Fprintf(h, "\x00%s\x00%x", c.Alias, sha256.Sum256(c.DER))
	}
	_, _ = fmt.Fprintf(h, "\x00%s", id)
	return certs, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (t *truststore) check(ctx context.Context) (bool, error) {
	certs, hash, err := t.inputs(ctx)
	if err != nil {
		return false, err
	}
	if len(certs) == 0 {
		return true, nil
	}
	vol, err := t.d.Docker.VolumeInspect(ctx, t.volume())
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state, err := t.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b := state.Truststore
	return b != nil && b.Hash == hash && b.VolumeCreatedAt == vol.CreatedAt, nil
}

func (t *truststore) apply(ctx context.Context, sink events.Sink) error {
	certs, hash, err := t.inputs(ctx)
	if err != nil || len(certs) == 0 {
		return err
	}
	vol, err := t.st.EnsureVolume(ctx, t.d.Docker, t.cfg.Name, t.volume(), truststoreVolume)
	if err != nil {
		return err
	}

	// The helper may replace the volume's contents and then fail, so forget
	// the old record first: restoring the old certs mustn't skip a rebuild.
	if err := t.record(nil); err != nil {
		return err
	}
	sink.Emit(events.Progress{ID: TruststoreStepID, Text: fmt.Sprintf("importing %d custom CA certificates into %s", len(certs), vol.Name)})
	var stdin bytes.Buffer
	args := []string{"-c", truststoreScript, "sh"}
	for _, c := range certs {
		_ = pem.Encode(&stdin, &pem.Block{Type: "CERTIFICATE", Bytes: c.DER})
		args = append(args, c.Alias)
	}
	stdout := events.NewLogWriter(sink, TruststoreStepID, events.StreamStdout)
	defer func() { _ = stdout.Close() }()
	stderr := events.NewLogWriter(sink, TruststoreStepID, events.StreamStderr)
	defer func() { _ = stderr.Close() }()
	name, err := docker.UniqueName(t.cfg.Name+"-truststore", t.d.Rand)
	if err != nil {
		return err
	}
	var errTail bytes.Buffer
	code, err := t.d.Docker.Run(ctx, docker.RunOpts{
		Image:      t.image,
		Args:       args,
		Name:       name,
		Remove:     true,
		User:       "0",
		Entrypoint: "sh",
		Network:    "none",
		Mounts:     []docker.Mount{{Source: vol.Name, Target: "/truststore"}},
		Labels:     t.st.Labels(t.cfg.Name),
		Stdin:      &stdin,
		Stdout:     stdout,
		Stderr:     io.MultiWriter(stderr, &errTail),
	})
	if err != nil {
		// docker run was interrupted, but its container may still be
		// writing the volume, where it would race a retry.
		_ = t.d.Docker.Rm(context.WithoutCancel(ctx), name, true)
	} else if code != 0 {
		err = fmt.Errorf("the helper container exited %d", code)
		if msg := strings.TrimSpace(errTail.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg[strings.LastIndexByte(msg, '\n')+1:])
		}
	}
	if err != nil {
		return fmt.Errorf("building the truststore in volume %s: %w", vol.Name, err)
	}

	return t.record(&stack.TruststoreBuild{Hash: hash, VolumeCreatedAt: vol.CreatedAt})
}

// record saves b as the truststore build in state.json, or forgets the
// build when b is nil.
func (t *truststore) record(b *stack.TruststoreBuild) error {
	state, err := t.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		state, err = &stack.State{}, nil
	}
	if err != nil {
		return err
	}
	if b == nil && state.Truststore == nil {
		return nil
	}
	state.Truststore = b
	return t.st.SaveState(state)
}
