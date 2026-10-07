package selfupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// Size limits on what a release may serve.
const (
	maxMetadata = 4 << 20   // release JSON, checksums.txt, the cosign bundle
	maxArchive  = 512 << 20 // the tar.gz, and the binary inside it
)

// release is the part of the GitHub releases API response pic-sure reads.
type release struct {
	Tag    string  `json:"tag_name"`
	Assets []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// find returns the asset called name.
func (r *release) find(name string) (asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return asset{}, false
}

// resolve fetches the release for version, or the latest release when
// version is empty.
func (u *Updater) resolve(ctx context.Context, version string) (*release, error) {
	what := "the latest pic-sure release"
	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase(), u.repo())
	if version != "" {
		what = "pic-sure release " + version
		endpoint = fmt.Sprintf("%s/repos/%s/releases/tags/%s", u.apiBase(), u.repo(), url.PathEscape(version))
	}
	body, status, err := u.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", what, err)
	}
	defer func() { _ = body.Close() }()
	if status == http.StatusNotFound {
		return nil, exitcode.Precondition("%s doesn't exist on github.com/%s", what, u.repo())
	}
	if status != http.StatusOK {
		return nil, exitcode.Failed("looking up %s: GitHub answered HTTP %d", what, status)
	}
	var r release
	if err := json.NewDecoder(io.LimitReader(body, maxMetadata)).Decode(&r); err != nil {
		return nil, exitcode.Failed("looking up %s: reading GitHub's answer: %v", what, err)
	}
	if r.Tag == "" {
		return nil, exitcode.Failed("looking up %s: GitHub's answer has no tag", what)
	}
	return &r, nil
}

// download writes asset a to the file dst and returns its SHA-256.
func (u *Updater) download(ctx context.Context, a asset, dst string, limit int64) (string, error) {
	body, status, err := u.get(ctx, a.URL, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	defer func() { _ = body.Close() }()
	if status != http.StatusOK {
		return "", exitcode.Failed("downloading %s: HTTP %d", a.Name, status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	if n > limit {
		return "", exitcode.Failed("downloading %s: larger than %d MiB", a.Name, limit>>20)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get sends a GET through the Updater's client. The caller closes the body.
// Errors name neither the URL nor the proxy, so no credential reaches a
// message.
func (u *Updater) get(ctx context.Context, rawURL, accept string) (io.ReadCloser, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, withoutURL(err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "pic-sure/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, 0, withoutURL(err)
	}
	return resp.Body, resp.StatusCode, nil
}

// withoutURL drops the URL a *url.Error adds to its cause.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// checksumFor returns the SHA-256 checksums.txt lists for name.
func checksumFor(checksums, name string) (string, error) {
	f, err := os.Open(checksums)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", exitcode.Failed("checksums.txt has no entry for %s", name)
}

// extractBinary copies the pic-sure binary out of the tar.gz archive into w.
func extractBinary(archive string, w io.Writer) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return exitcode.Failed("reading the release archive: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return exitcode.Failed("the release archive has no pic-sure binary")
		}
		if err != nil {
			return exitcode.Failed("reading the release archive: %v", err)
		}
		if path.Clean(h.Name) != binaryName || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxArchive {
			return exitcode.Failed("the release archive's pic-sure binary is larger than %d MiB", maxArchive>>20)
		}
		if _, err := io.Copy(w, io.LimitReader(tr, h.Size)); err != nil {
			return fmt.Errorf("extracting pic-sure: %w", err)
		}
		return nil
	}
}
