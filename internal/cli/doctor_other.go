//go:build !unix

package cli

import "errors"

func diskFree(string) (uint64, error) { return 0, errors.New("not supported on this OS") }
