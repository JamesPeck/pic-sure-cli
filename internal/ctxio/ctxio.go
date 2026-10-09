// Package ctxio makes long reads stop when their context ends.
package ctxio

import (
	"context"
	"io"
)

// Reader returns a reader of r that fails with ctx's cause once ctx is
// done, so a long copy or extraction ends promptly on Ctrl-C.
func Reader(ctx context.Context, r io.Reader) io.Reader {
	return reader{ctx, r}
}

type reader struct {
	ctx context.Context
	r   io.Reader
}

func (c reader) Read(p []byte) (int, error) {
	if err := context.Cause(c.ctx); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
