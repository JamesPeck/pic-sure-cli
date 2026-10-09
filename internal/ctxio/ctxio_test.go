package ctxio_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/ctxio"
)

func TestReaderStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	r := ctxio.Reader(ctx, strings.NewReader("abcdef"))
	buf := make([]byte, 3)
	if n, err := r.Read(buf); n != 3 || err != nil {
		t.Fatalf("Read before cancel = %d, %v", n, err)
	}
	stop := errors.New("interrupted")
	cancel(stop)
	if _, err := io.ReadAll(r); !errors.Is(err, stop) {
		t.Errorf("Read after cancel: %v, want the cause", err)
	}
}
