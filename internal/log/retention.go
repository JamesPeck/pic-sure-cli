package log

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// A stack keeps its newest run logs up to both limits: 50 files and 50 MiB,
// whichever is reached first (spec §6.1).
const (
	maxFiles = 50
	maxBytes = 50 << 20
)

// prune deletes run logs (cli-*.log) in stackDir's Dir so that the newest
// that fit in maxFiles and maxBytes remain. current, the running command's
// log, is always kept and counts toward both limits. Logs are ordered by
// name, which starts with their UTC timestamp. Other files, directories and
// symlinks are left alone.
func prune(stackDir, current string, maxFiles int, maxBytes int64) error {
	root, err := os.OpenRoot(stackDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dir, err := root.OpenRoot(Dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}

	type runLog struct {
		name string
		size int64
	}
	var logs []runLog
	kept, total := 0, int64(0)
	for _, e := range entries {
		if !e.Type().IsRegular() || !isRunLog(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed since ReadDir, likely by another run's prune
		}
		if e.Name() == current {
			kept, total = 1, info.Size()
			continue
		}
		logs = append(logs, runLog{e.Name(), info.Size()})
	}
	slices.SortFunc(logs, func(a, b runLog) int { return strings.Compare(b.name, a.name) })

	var errs []error
	full := false
	for _, l := range logs {
		if !full && kept < maxFiles && total+l.size <= maxBytes {
			kept++
			total += l.size
			continue
		}
		// Once one log doesn't fit, every older one goes too.
		full = true
		if err := dir.Remove(l.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isRunLog(name string) bool {
	return strings.HasPrefix(name, "cli-") && strings.HasSuffix(name, ".log")
}
