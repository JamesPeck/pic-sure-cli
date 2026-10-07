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

// prune deletes run logs (cli-*.log) in st's Dir so that the newest that
// fit in maxFiles and maxBytes remain. current, the running command's log,
// is always kept and counts toward both limits. Logs are ordered by their
// names' UTC timestamps. Logs st doesn't own, other files, directories and
// symlinks are left alone and don't count.
func prune(st Store, current string, maxFiles int, maxBytes int64) error {
	entries, err := os.ReadDir(st.Path(Dir))
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
		if !e.Type().IsRegular() || !isRunLog(e.Name()) || !st.Owns(Dir+"/"+e.Name()) {
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
	// Newest first. Without ".log", a collision's cli-<ts>-<pid>-1 sorts
	// after cli-<ts>-<pid>.
	slices.SortFunc(logs, func(a, b runLog) int {
		return strings.Compare(strings.TrimSuffix(b.name, ".log"), strings.TrimSuffix(a.name, ".log"))
	})

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
		if err := st.Remove(Dir + "/" + l.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isRunLog(name string) bool {
	return strings.HasPrefix(name, "cli-") && strings.HasSuffix(name, ".log")
}
