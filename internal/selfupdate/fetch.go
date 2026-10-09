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
	"regexp"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// Size limits on what a release may serve.
const (
	maxMetadata    = 4 << 20   // release JSON, checksums.txt, the cosign bundle
	maxReleasePage = 32 << 20  // one page of the release list
	maxArchive     = 512 << 20 // the tar.gz, and the binary inside it
)

// release is the part of the GitHub releases API response pic-sure reads.
type release struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
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

// Release listing: GitHub's largest page, and how many pages to read.
const (
	releasesPerPage = 100
	maxReleasePages = 10
)

// stableV2 admits only tags stack.CompareVersions can order: no leading
// zeros, and components small enough for an int.
var stableV2 = regexp.MustCompile(`^v2\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

var errNotFound = errors.New("not found")

// resolve fetches the release for version, or the newest stable v2 release
// when version is empty.
func (u *Updater) resolve(ctx context.Context, version string) (*release, error) {
	if version == "" {
		return u.newest(ctx)
	}
	what := "pic-sure release " + version
	var r release
	endpoint := fmt.Sprintf("%s/repos/%s/releases/tags/%s", u.apiBase(), u.repo(), url.PathEscape(version))
	if err := u.getJSON(ctx, endpoint, what, maxMetadata, &r); errors.Is(err, errNotFound) {
		return nil, exitcode.Precondition("%s doesn't exist on github.com/%s", what, u.repo())
	} else if err != nil {
		return nil, err
	}
	if r.Tag == "" {
		return nil, exitcode.Failed("looking up %s: GitHub's answer has no tag", what)
	}
	if r.Tag != version {
		return nil, exitcode.Failed("looking up %s: GitHub answered with release %s instead; not installing it", what, r.Tag)
	}
	return &r, nil
}

// newest lists the repo's releases and returns the highest vX.Y.Z with
// major 2 that is neither a draft nor a prerelease. GitHub's
// releases/latest is whichever release was published last, which may be a
// backport to an older v2.x or a release from another line. install.sh
// picks its default version by the same rule.
func (u *Updater) newest(ctx context.Context) (*release, error) {
	const what = "the newest pic-sure release"
	var best *release
	for page := 1; page <= maxReleasePages; page++ {
		var rels []release
		endpoint := fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", u.apiBase(), u.repo(), releasesPerPage, page)
		if err := u.getJSON(ctx, endpoint, what, maxReleasePage, &rels); errors.Is(err, errNotFound) {
			return nil, exitcode.Precondition("github.com/%s doesn't exist", u.repo())
		} else if err != nil {
			return nil, err
		}
		for i := range rels {
			r := &rels[i]
			if r.Draft || r.Prerelease || !stableV2.MatchString(r.Tag) {
				continue
			}
			if best == nil {
				best = r
			} else if order, _ := stack.CompareVersions(r.Tag, best.Tag); order > 0 {
				best = r
			}
		}
		if len(rels) < releasesPerPage {
			break
		}
	}
	if best == nil {
		return nil, exitcode.Precondition("github.com/%s has no stable v2.x.y release; pass --to to choose one", u.repo())
	}
	return best, nil
}

// getJSON decodes the GitHub API's answer at endpoint, at most limit bytes,
// into v. what names the lookup in errors. An HTTP 404 is errNotFound.
func (u *Updater) getJSON(ctx context.Context, endpoint, what string, limit int64, v any) error {
	body, status, err := u.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return fmt.Errorf("looking up %s: %w", what, err)
	}
	defer func() { _ = body.Close() }()
	if status == http.StatusNotFound {
		return errNotFound
	}
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return exitcode.Failed("looking up %s: GitHub answered HTTP %d, probably its rate limit "+
			"for unauthenticated requests; try again later", what, status)
	}
	if status != http.StatusOK {
		return exitcode.Failed("looking up %s: GitHub answered HTTP %d", what, status)
	}
	if err := json.NewDecoder(io.LimitReader(body, limit)).Decode(v); err != nil {
		return exitcode.Failed("looking up %s: reading GitHub's answer: %v", what, err)
	}
	return nil
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

// stallTimeout is how long a request may go without receiving anything
// before it is abandoned.
var stallTimeout = time.Minute

var errStalled = errors.New("the server stopped sending data")

// get sends a GET through the Updater's client. The caller closes the body.
// Errors name neither the URL nor the proxy, so no credential reaches a
// message.
func (u *Updater) get(ctx context.Context, rawURL, accept string) (io.ReadCloser, int, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err == nil {
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", "pic-sure/"+u.Current)
		var resp *http.Response
		if resp, err = u.client().Do(req); err == nil {
			return &watchedBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timer: timer}, resp.StatusCode, nil
		}
	}
	timer.Stop()
	cancel(nil)
	if errors.Is(context.Cause(ctx), errStalled) {
		return nil, 0, errStalled
	}
	return nil, 0, withoutURL(err)
}

// watchedBody is a response body whose request is cancelled when no data
// arrives for stallTimeout.
type watchedBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(stallTimeout)
	}
	if err != nil && errors.Is(context.Cause(b.ctx), errStalled) {
		err = errStalled
	}
	return n, err
}

func (b *watchedBody) Close() error {
	b.timer.Stop()
	b.cancel(nil)
	return b.ReadCloser.Close()
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
