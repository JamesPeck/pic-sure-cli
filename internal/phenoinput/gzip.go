package phenoinput

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

var errTrailingData = errors.New("gzip: data after the end of the compressed stream")

// gzipReader decompresses every member of a gzip stream, like gzip.Reader
// in its default multistream mode, but treats zero bytes after the last
// member as padding, as GNU tar does, rather than as a corrupt header.
// Each member's checksum is verified when its end is read.
type gzipReader struct {
	br *bufio.Reader
	z  *gzip.Reader
}

func newGzipReader(r io.Reader) (*gzipReader, error) {
	// A bufio.Reader is an io.ByteReader, so gzip leaves it positioned just
	// after each member instead of reading ahead into the next.
	br := bufio.NewReader(r)
	z, err := gzip.NewReader(br)
	if err != nil {
		return nil, err
	}
	z.Multistream(false)
	return &gzipReader{br: br, z: z}, nil
}

func (g *gzipReader) Read(p []byte) (int, error) {
	for {
		n, err := g.z.Read(p)
		if err != io.EOF {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
		more, err := g.nextMember()
		if err != nil {
			return 0, err
		}
		if !more {
			return 0, io.EOF
		}
	}
}

// nextMember starts reading the member after the one just finished, and
// reports false at the end of the stream.
func (g *gzipReader) nextMember() (bool, error) {
	b, err := g.br.Peek(len(gzipMagic))
	switch {
	case len(b) == 0 && err == io.EOF:
		return false, nil
	case len(b) > 0 && b[0] == 0:
		return false, skipZeroPadding(g.br)
	case bytes.Equal(b, gzipMagic):
	case err != nil && err != io.EOF:
		return false, err
	default:
		return false, errTrailingData
	}
	if err := g.z.Reset(g.br); err != nil {
		return false, err
	}
	g.z.Multistream(false)
	return true, nil
}

func skipZeroPadding(br *bufio.Reader) error {
	for {
		c, err := br.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if c != 0 {
			return errTrailingData
		}
	}
}

func (g *gzipReader) Close() error { return g.z.Close() }
