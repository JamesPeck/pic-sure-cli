package docker

import (
	"strings"
	"testing"
	"testing/iotest"
)

func TestContainsStreamAcrossReads(t *testing.T) {
	const log = "Starting\nStarted DictionaryEtlApplication in 9.1 seconds\n"
	r := iotest.OneByteReader(strings.NewReader(log))
	if found, err := containsStream(r, []byte("Started DictionaryEtlApplication")); !found || err != nil {
		t.Errorf("one byte per read: %v, %v", found, err)
	}
}
