package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

func (e *cliEngine) Logs(ctx context.Context, container string, follow bool) io.ReadCloser {
	argv := []string{"docker", "logs"}
	if follow {
		argv = append(argv, "-f")
	}
	argv = append(argv, container)

	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &logStream{pr: pr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		code, stderr, err := e.stream(ctx, Cmd{Argv: argv}, pw, pw)
		if err == nil && code != 0 {
			err = exitError(argv, code, stderr)
		}
		pw.CloseWithError(err) // nil gives the reader io.EOF
	}()
	return s
}

// logStream is the reading end of a `docker logs` run.
type logStream struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *logStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

// Close stops docker logs and waits for it to exit.
func (s *logStream) Close() error {
	s.cancel()
	err := s.pr.Close()
	<-s.done
	return err
}

func (e *cliEngine) WaitForLogLine(ctx context.Context, container, substr string, timeout time.Duration) error {
	if substr == "" {
		return errors.New("WaitForLogLine: empty substring")
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	logs := e.Logs(wctx, container, true)
	defer func() { _ = logs.Close() }()

	found, err := containsStream(logs, []byte(substr))
	switch {
	case found:
		return nil
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case wctx.Err() != nil:
		return fmt.Errorf("%s did not log %q within %s: %w", container, substr, timeout, wctx.Err())
	case err != nil:
		return err
	}
	return fmt.Errorf("%s stopped without logging %q", container, substr)
}

// containsStream reads r until it finds needle, which must not be empty, or
// r ends. Lines may be any length: it keeps only enough of the previous read
// to catch a match that straddles two reads.
func containsStream(r io.Reader, needle []byte) (bool, error) {
	buf := make([]byte, 32<<10)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			if bytes.Contains(window, needle) {
				return true, nil
			}
			keep := min(len(needle)-1, len(window))
			carry = append([]byte(nil), window[len(window)-keep:]...)
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
