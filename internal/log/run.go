package log

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Dir is where a stack keeps its run logs, relative to the stack directory.
const Dir = ".pic-sure/logs"

// Store is the stack directory a run keeps its log files in, with paths
// relative to it. *stack.Stack is one: it confines the writes to the stack
// and records what it creates in the stack's manifest, so destroy removes
// the logs (§6.3).
type Store interface {
	MkdirAll(rel string, perm fs.FileMode) error
	// CreateFile creates the new file rel; it fails with fs.ErrExist if
	// rel exists.
	CreateFile(rel string, perm fs.FileMode) (*os.File, error)
	Remove(rel string) error
	// Path returns rel as a path the os package can open.
	Path(rel string) string
}

// maxEarly bounds the records a run holds in memory before it knows where
// its log file goes. Past it, early records are dropped and counted.
const maxEarly = 1 << 20

// ParseLevel parses a --log-level value: debug, info, warn or error, in any
// case.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
}

// Options configure one command run's logging.
type Options struct {
	// Level is the stderr level, from --log-level.
	Level slog.Level
	// Stderr receives records at Level and above, as text.
	Stderr io.Writer
	// File says whether the run may write a log file. Without it, OpenFile
	// does nothing.
	File bool
	// Redactor holds the secret values to scrub from all output. Nil means
	// the process-wide registry (RegisterSecrets).
	Redactor *Redactor
}

// Run is the logging for one command run. Records at the --log-level go to
// stderr as text. With Options.File, every record down to debug also goes
// to the stack's per-run log file as JSON lines, once the command knows its
// stack and calls OpenFile; records from before then are held in memory and
// written first. Both outputs pass through the Redactor and redact
// secret-named attrs (IsSecretName).
type Run struct {
	logger *slog.Logger
	file   *fileSink // nil without Options.File
}

// New starts a run's logging.
func New(opts Options) *Run {
	red := opts.Redactor
	if red == nil {
		red = &registry
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	var h slog.Handler = slog.NewTextHandler(redactWriter{r: red, w: stderr}, &slog.HandlerOptions{Level: opts.Level})
	run := &Run{}
	if opts.File {
		run.file = &fileSink{redactor: red}
		h = fanout{h, slog.NewJSONHandler(run.file, &slog.HandlerOptions{Level: slog.LevelDebug})}
	}
	run.logger = slog.New(nameHandler{inner: h})
	return run
}

// Logger returns the run's logger.
func (r *Run) Logger() *slog.Logger { return r.logger }

// OpenFile starts the run's log file in st's Dir, named cli-<now in
// UTC>.log, writes the records logged so far to it, and prunes old run logs
// (prune). It returns the file's path, or "" when the run writes no file
// (Options.File is false, or the run is closed). Once a file is open, later
// calls return its path. Failing to prune is logged, not returned.
func (r *Run) OpenFile(st Store, now time.Time) (string, error) {
	if r.file == nil {
		return "", nil
	}
	path, dropped, opened, err := r.file.open(st, now)
	if err != nil || !opened {
		return path, err
	}
	if dropped > 0 {
		r.logger.Debug("dropped early log records", "count", dropped)
	}
	if err := prune(st, filepath.Base(path), maxFiles, maxBytes); err != nil {
		r.logger.Warn("can't prune old run logs", "err", err)
	}
	return path, nil
}

// Close closes the run's log file. Records logged after Close still go to
// stderr.
func (r *Run) Close() error {
	if r.file == nil {
		return nil
	}
	return r.file.close()
}

// fileSink is the JSON handler's writer: an in-memory buffer until the file
// is open, then the file.
type fileSink struct {
	redactor *Redactor

	mu      sync.Mutex
	early   []byte // records written before open
	dropped int    // early records that didn't fit in maxEarly
	f       *os.File
	path    string
	closed  bool
}

func (s *fileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.f != nil:
		return redactWriter{r: s.redactor, w: s.f}.Write(p)
	case s.closed:
	case len(s.early)+len(p) <= maxEarly:
		s.early = append(s.early, p...)
	default:
		s.dropped++
	}
	return len(p), nil
}

// open creates the log file in st and flushes the early records into it,
// redacting them with the secrets registered by now. It reports whether it
// opened a file: not if one is already open or the sink is closed.
func (s *fileSink) open(st Store, now time.Time) (path string, dropped int, opened bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil || s.closed {
		return s.path, 0, false, nil
	}
	if err := st.MkdirAll(Dir, 0o700); err != nil {
		return "", 0, false, err
	}
	name, f, err := createRunFile(st, now)
	if err != nil {
		return "", 0, false, err
	}
	if len(s.early) > 0 {
		if _, err := (redactWriter{r: s.redactor, w: f}).Write(s.early); err != nil {
			_ = f.Close()
			return "", 0, false, err
		}
	}
	s.f, s.path = f, st.Path(Dir+"/"+name)
	dropped = s.dropped
	s.early, s.dropped = nil, 0
	return s.path, dropped, true, nil
}

func (s *fileSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.early = nil
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// createRunFile creates Dir/cli-<ts>.log in st, mode 0600, adding a counter
// if another run took the name in the same millisecond.
func createRunFile(st Store, now time.Time) (string, *os.File, error) {
	ts := now.UTC().Format("20060102T150405.000Z")
	for i := 0; ; i++ {
		name := "cli-" + ts + ".log"
		if i > 0 {
			name = fmt.Sprintf("cli-%s-%d.log", ts, i)
		}
		f, err := st.CreateFile(Dir+"/"+name, 0o600)
		if errors.Is(err, fs.ErrExist) && i < 100 {
			continue
		}
		return name, f, err
	}
}

// fanout sends each record to every handler that wants it.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
