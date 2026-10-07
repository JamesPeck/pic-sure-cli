package ops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/pki"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// TLSStepID is the TLS step's ID, for --skip-step.
const TLSStepID = "tls"

// TLSDir is where generated mode keeps the key and certificate it makes:
// the source for the certs volume.
const TLSDir = stack.CLIDir + "/tls"

// tlsRenewBefore is how close to expiry a generated certificate is replaced.
const tlsRenewBefore = 30 * 24 * time.Hour

// tlsInstallScript runs in the helper container, with the certs volume at
// /certs and a tar of the three files on stdin. It unpacks them beside
// their final place, hands them to httpd's uid and gid 2, and renames them
// over the old ones, so httpd never reads a half-written file.
const tlsInstallScript = `set -eu
cd /certs
rm -rf .incoming
mkdir -m 0700 .incoming
tar -x -m -o -f - -C .incoming
cd .incoming
chown 2:2 server.key server.crt server.chain
chmod 0640 server.key
chmod 0644 server.crt server.chain
mv -f server.key server.crt server.chain ..
cd ..
rmdir .incoming
`

// TLSStep returns the step that puts httpd's server.key, server.crt and
// server.chain in the stack's certs volume (§9.1 step 6), owned by uid and
// gid 2 with modes 0640 and 0644, so httpd can read the key without a host
// chgrp.
//
// In generated mode the files come from TLSDir. The step reuses the ones
// there while they are valid, name the configured hostname and are more
// than 30 days from expiry, and otherwise makes a new self-signed
// certificate. In provided mode it validates the operator's files and
// copies those; without a chain file the certificate file is the chain,
// because the vhost always names one. Validation warnings, such as a
// certificate that doesn't name the hostname, become Warning events.
//
// The step is done when state.json records the same files copied into the
// same volume. It doesn't restart httpd, which reads the files only at
// start.
func TLSStep(d *Deps, st *stack.Stack, cfg *stack.Config) steps.Step {
	t := &tlsStep{d: d, st: st, cfg: cfg}
	return steps.Step{
		ID:    TLSStepID,
		Title: "Install the TLS certificate",
		Check: t.check,
		Apply: t.apply,
	}
}

type tlsStep struct {
	d   *Deps
	st  *stack.Stack
	cfg *stack.Config
}

func (t *tlsStep) volume() string {
	v, _ := catalog.LookupVolume("certs")
	return v.DockerName(t.cfg.Name)
}

func (t *tlsStep) check(ctx context.Context) (bool, error) {
	state, err := t.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state.TLS == nil {
		return false, nil
	}

	var files pki.Files
	if t.cfg.TLS.Mode == stack.TLSProvided {
		// Apply reports a file that can't be read. An installed certificate
		// isn't re-validated, so one that has expired since doesn't stop
		// the stack from starting.
		if files, err = t.providedFiles(); err != nil {
			return false, nil
		}
	} else {
		var reason string
		if files, reason, err = t.generatedFiles(); err != nil || reason != "" {
			return false, err
		}
	}
	archive, err := tlsArchive(files)
	if err != nil {
		return false, err
	}
	if tlsHash(archive) != state.TLS.Hash {
		return false, nil
	}

	vol, err := t.d.Docker.VolumeInspect(ctx, t.volume())
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return vol.CreatedAt == state.TLS.VolumeCreatedAt, nil
}

func (t *tlsStep) apply(ctx context.Context, sink events.Sink) error {
	files, err := t.source(sink)
	if err != nil {
		return err
	}
	archive, err := tlsArchive(files)
	if err != nil {
		return err
	}
	vol, err := t.st.EnsureVolume(ctx, t.d.Docker, t.cfg.Name, t.volume(), "certs")
	if err != nil {
		return err
	}

	// Forget the old install before the helper replaces it, so an
	// interrupted copy isn't taken for the files it was replacing.
	state, err := t.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		state, err = &stack.State{}, nil
	}
	if err != nil {
		return err
	}
	if state.TLS != nil {
		state.TLS = nil
		if err := t.st.SaveState(state); err != nil {
			return err
		}
	}

	name, err := docker.UniqueName(t.cfg.Name+"-tls", t.d.Rand)
	if err != nil {
		return err
	}
	alpine, _ := catalog.LookupImage("alpine")
	var stderr bytes.Buffer
	code, err := t.d.Docker.Run(ctx, docker.RunOpts{
		Image:   alpine.Ref,
		Name:    name,
		Remove:  true,
		Network: "none",
		Labels:  t.st.Labels(t.cfg.Name),
		Mounts:  []docker.Mount{{Source: vol.Name, Target: "/certs"}},
		Args:    []string{"sh", "-c", tlsInstallScript},
		Stdin:   bytes.NewReader(archive),
		Stderr:  &stderr,
	})
	if err == nil && code != 0 {
		err = fmt.Errorf("the helper container exited %d", code)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg[strings.LastIndexByte(msg, '\n')+1:])
		}
	}
	if err != nil {
		return fmt.Errorf("copying the TLS files into volume %s: %w", vol.Name, err)
	}

	state.TLS = &stack.TLSInstall{Hash: tlsHash(archive), VolumeCreatedAt: vol.CreatedAt}
	return t.st.SaveState(state)
}

// source returns the files to install: the operator's, validated, in
// provided mode; otherwise the generated ones, made anew when those in
// TLSDir won't do.
func (t *tlsStep) source(sink events.Sink) (pki.Files, error) {
	hostname, now := t.cfg.Network.Hostname, t.d.Clock.Now()
	if t.cfg.TLS.Mode == stack.TLSProvided {
		files, err := t.providedFiles()
		if err != nil {
			return pki.Files{}, err
		}
		report, err := pki.Validate(files, hostname, now)
		if err != nil {
			return pki.Files{}, fmt.Errorf("tls.mode is provided, but the files won't do: %w", err)
		}
		for _, w := range report.Warnings {
			sink.Emit(events.Warning{ID: TLSStepID, Text: w})
		}
		return files, nil
	}

	files, reason, err := t.generatedFiles()
	if err != nil || reason == "" {
		return files, err
	}
	text := "Generating a self-signed certificate for " + hostname
	if reason != tlsMissing {
		text += " to replace the one in " + TLSDir + ", which " + reason
	}
	sink.Emit(events.Progress{ID: TLSStepID, Text: text})
	files, err = pki.Generate(t.d.Rand, hostname, now)
	if err != nil {
		return pki.Files{}, fmt.Errorf("network.hostname: %w", err)
	}
	if err := t.st.MkdirAll(TLSDir, 0o700); err != nil {
		return pki.Files{}, err
	}
	for _, f := range tlsFiles(&files) {
		if err := t.st.WriteFile(TLSDir+"/"+f.name, *f.data, 0o600); err != nil {
			return pki.Files{}, err
		}
	}
	return files, nil
}

// tlsMissing is generatedFiles' reason when there is no certificate yet.
const tlsMissing = "is missing"

// generatedFiles reads the files in TLSDir. If they won't do, reason says
// why: they are missing (tlsMissing), don't validate, don't name the
// hostname, or expire within tlsRenewBefore.
func (t *tlsStep) generatedFiles() (files pki.Files, reason string, err error) {
	for _, f := range tlsFiles(&files) {
		data, err := t.st.ReadFile(TLSDir + "/" + f.name)
		if errors.Is(err, fs.ErrNotExist) {
			return pki.Files{}, tlsMissing, nil
		}
		if err != nil {
			return pki.Files{}, "", err
		}
		*f.data = data
	}
	hostname, now := t.cfg.Network.Hostname, t.d.Clock.Now()
	report, err := pki.Validate(files, hostname, now)
	switch {
	case err != nil:
		return pki.Files{}, "isn't valid (" + strings.ReplaceAll(err.Error(), "\n", "; ") + ")", nil
	case len(report.Warnings) > 0:
		return pki.Files{}, fmt.Sprintf("doesn't name %s", hostname), nil
	case report.NotAfter.Sub(now) < tlsRenewBefore:
		return pki.Files{}, "expires on " + report.NotAfter.UTC().Format(time.DateOnly), nil
	}
	return files, "", nil
}

// providedFiles reads the operator's files that the tls block names. With
// no chain file, the certificate file serves as the chain. A blank chain
// file is refused: httpd won't start on a zero-byte one, and one that is
// only whitespace holds no chain.
func (t *tlsStep) providedFiles() (pki.Files, error) {
	read := func(key, p string) ([]byte, error) {
		if !filepath.IsAbs(p) {
			p = t.st.Path(p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		return data, nil
	}
	tls := t.cfg.TLS
	var f pki.Files
	var err error
	if f.Key, err = read("tls.key_file", tls.KeyFile); err != nil {
		return pki.Files{}, err
	}
	if f.Cert, err = read("tls.cert_file", tls.CertFile); err != nil {
		return pki.Files{}, err
	}
	if tls.ChainFile == "" {
		f.Chain = f.Cert
	} else if f.Chain, err = read("tls.chain_file", tls.ChainFile); err != nil {
		return pki.Files{}, err
	} else if len(bytes.TrimSpace(f.Chain)) == 0 {
		return pki.Files{}, fmt.Errorf("tls.chain_file: %s is empty; leave chain_file blank if there is no chain", tls.ChainFile)
	}
	return f, nil
}

type tlsFile struct {
	name string
	data *[]byte
}

// tlsFiles pairs each of f's fields with its file name in the certs volume
// and in TLSDir.
func tlsFiles(f *pki.Files) []tlsFile {
	return []tlsFile{{"server.key", &f.Key}, {"server.crt", &f.Cert}, {"server.chain", &f.Chain}}
}

// tlsArchive is the tar the helper unpacks. Each entry has the same mode and
// time, so the same files always make the same bytes.
func tlsArchive(f pki.Files) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range tlsFiles(&f) {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Mode:     0o600,
			Size:     int64(len(*e.data)),
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(*e.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tlsHash identifies an install by the archive and the script that unpacks
// it, so changing how the files are installed installs them again.
func tlsHash(archive []byte) string {
	h := sha256.New()
	h.Write([]byte(tlsInstallScript))
	h.Write([]byte{0})
	h.Write(archive)
	return hex.EncodeToString(h.Sum(nil))
}
