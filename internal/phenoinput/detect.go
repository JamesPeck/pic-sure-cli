package phenoinput

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// sniffLen is how much of a file, or of its decompressed content, detect
// reads to classify it.
const sniffLen = 8 << 10

const tarBlockSize = 512

var (
	gzipMagic     = []byte{0x1f, 0x8b}
	zipMagic      = []byte("PK\x03\x04")
	zipEmptyMagic = []byte("PK\x05\x06") // an empty zip is only its end-of-directory record
)

// unsupported are compressed formats recognised only to reject them with a
// clear message, by their leading magic bytes. Their data needn't contain a
// NUL byte, so isBinary alone could mistake a small one for text.
var unsupported = []struct {
	name  string
	magic []byte
}{
	{"xz", []byte("\xfd7zXZ\x00")},
	{"zstd", []byte("\x28\xb5\x2f\xfd")},
	{"lz4", []byte("\x04\x22\x4d\x18")},
	{"7-Zip", []byte("7z\xbc\xaf\x27\x1c")},
}

// detect classifies file by its content.
func detect(ctx context.Context, file string) (Format, error) {
	st, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", inputErr("%s is not a regular file", file)
	}
	if st.Size() == 0 {
		return "", inputErr("%s is empty", file)
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	head, err := sniff(ctxReader{ctx, f})
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", file, err)
	}

	switch {
	case bytes.HasPrefix(head, gzipMagic):
		return detectGzip(ctx, file, f)
	case bytes.HasPrefix(head, zipMagic), bytes.HasPrefix(head, zipEmptyMagic):
		return Zip, nil
	case looksLikeTar(head):
		return Tar, nil
	}
	if name := unsupportedFormat(head); name != "" {
		return "", inputErr("%s is %s-compressed, which isn't supported; use a CSV, gzip, tar or zip file", file, name)
	}
	if isBinary(head) {
		return "", inputErr("%s is neither a CSV nor a gzip, tar or zip archive", file)
	}
	return CSV, nil
}

// detectGzip tells a gzipped tar from a gzip of a single CSV by
// decompressing the start of f.
func detectGzip(ctx context.Context, file string, f *os.File) (Format, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	gz, err := newGzipReader(ctxReader{ctx, f})
	if err != nil {
		return "", inputErr("reading %s as gzip: %w", file, err)
	}
	defer func() { _ = gz.Close() }()
	head, err := sniff(gz)
	switch {
	case err != nil:
		return "", inputErr("decompressing %s: %w", file, err)
	case len(head) == 0:
		return "", inputErr("%s decompresses to an empty file", file)
	case looksLikeTar(head):
		return TarGz, nil
	case isBinary(head):
		return "", inputErr("%s decompresses to binary data, not a CSV or a tar archive", file)
	}
	return Gzip, nil
}

// sniff reads up to sniffLen bytes of r. Reaching the end of r early is not
// an error; any other read error is.
func sniff(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, sniffLen))
}

// isBinary reports whether head, the start of a file, holds a NUL byte,
// which text never does. This is what grep -I checks in the C locale.
func isBinary(head []byte) bool {
	return bytes.IndexByte(head, 0) >= 0
}

// looksLikeTar reports whether head starts with a tar header block: one
// whose checksum is valid, or the all-zero block that ends a tar (all an
// empty tar has).
func looksLikeTar(head []byte) bool {
	if len(head) < tarBlockSize {
		return false
	}
	block := head[:tarBlockSize]
	var zero [tarBlockSize]byte
	return bytes.Equal(block, zero[:]) || tarChecksumOK(block)
}

// tarChecksumOK checks a tar header's checksum field (octal, at bytes
// 148–155), which is the sum of the header's bytes with that field read as
// spaces. Some old tars summed signed bytes, so either sum is accepted.
func tarChecksumOK(block []byte) bool {
	field := strings.Trim(string(block[148:156]), " \x00")
	want, err := strconv.ParseInt(field, 8, 64)
	if err != nil {
		return false
	}
	var unsigned, signed int64
	for i, c := range block {
		if i >= 148 && i < 156 {
			c = ' '
		}
		unsigned += int64(c)
		signed += int64(int8(c))
	}
	return want == unsigned || want == signed
}

// unsupportedFormat names the unsupported compressed format head starts
// with, or returns "".
func unsupportedFormat(head []byte) string {
	for _, u := range unsupported {
		if bytes.HasPrefix(head, u.magic) {
			return u.name
		}
	}
	if isBzip2(head) {
		return "bzip2"
	}
	return ""
}

// isBzip2 matches "BZh", the block size digit, and the magic of the first
// block or, for empty input, of the end of the stream. Checking all ten
// bytes keeps a text file that happens to start with "BZh" a CSV.
func isBzip2(head []byte) bool {
	if len(head) < 10 || !bytes.HasPrefix(head, []byte("BZh")) || head[3] < '1' || head[3] > '9' {
		return false
	}
	magic := string(head[4:10])
	return magic == "1AY&SY" || magic == "\x17rE8P\x90"
}
