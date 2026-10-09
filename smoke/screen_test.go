package smoke

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// screen is a minimal terminal emulator, enough to turn what Bubble Tea
// writes into the text a user would see: cursor moves, erases, tabs and
// REP. Colors and modes are ignored.
type screen struct {
	rows, cols int
	cells      [][]rune
	r, c       int
	last       rune
	// top and bottom are the scrolling region's rows.
	top, bottom int
	// pending is the incomplete escape sequence or UTF-8 rune that ended
	// the last write, kept for the next.
	pending []byte
}

func newScreen(rows, cols int) *screen {
	s := &screen{rows: rows, cols: cols, bottom: rows - 1}
	s.clear()
	return s
}

func (s *screen) blank() []rune { return []rune(strings.Repeat(" ", s.cols)) }

// scroll moves rows from..bottom up n lines (down when n < 0), blanking
// what is uncovered.
func (s *screen) scroll(from, n int) {
	region := s.cells[from : s.bottom+1]
	for range max(n, -n) {
		if n > 0 {
			copy(region, region[1:])
			region[len(region)-1] = s.blank()
		} else {
			copy(region[1:], region)
			region[0] = s.blank()
		}
	}
}

func (s *screen) clear() {
	s.cells = make([][]rune, s.rows)
	for i := range s.cells {
		s.cells[i] = []rune(strings.Repeat(" ", s.cols))
	}
}

func (s *screen) put(ch rune) {
	if s.c >= s.cols {
		s.c = 0
		s.lineFeed()
	}
	s.cells[s.r][s.c] = ch
	s.c++
	s.last = ch
}

func (s *screen) lineFeed() {
	switch {
	case s.r == s.bottom:
		s.scroll(s.top, 1)
	case s.r < s.rows-1:
		s.r++
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// write feeds the screen output as read from the PTY, which may split an
// escape sequence or a rune between reads.
func (s *screen) write(b []byte) {
	if len(s.pending) > 0 {
		b = append(s.pending, b...)
		s.pending = nil
	}
	for i := 0; i < len(b); {
		n := s.step(b[i:])
		if n == 0 {
			s.pending = append([]byte(nil), b[i:]...)
			return
		}
		i += n
	}
}

// step interprets the control, escape sequence or rune that b starts with
// and returns its length, or 0 if b ends before it does.
func (s *screen) step(b []byte) int {
	switch ch := b[0]; {
	case ch == 0x1b:
		return s.escape(b)
	case ch == '\r':
		s.c = 0
	case ch == '\n':
		s.lineFeed()
	case ch == '\t':
		s.c = min((s.c/8+1)*8, s.cols-1)
	case ch == '\b':
		s.c = max(s.c-1, 0)
	case ch < 0x20:
	default:
		if !utf8.FullRune(b) {
			return 0
		}
		r, n := utf8.DecodeRune(b)
		s.put(r)
		return n
	}
	return 1
}

// escape interprets the escape sequence b starts with, like step.
func (s *screen) escape(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	switch b[1] {
	case '[':
		for j := 2; j < len(b); j++ {
			if b[j] >= 0x40 && b[j] <= 0x7e {
				s.csi(string(b[2:j]), b[j])
				return j + 1
			}
		}
	case ']', 'P', '_':
		// OSC, DCS, APC: up to BEL or ST.
		for j := 2; j < len(b); j++ {
			if b[j] == 0x07 {
				return j + 1
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
		}
	default:
		// ESC, intermediate bytes, then the final byte, e.g. ESC ( B.
		for j := 1; j < len(b); j++ {
			if b[j] == 0x1b {
				return j // a new sequence cancels this one
			}
			if b[j] < 0x20 || b[j] > 0x2f {
				return j + 1
			}
		}
	}
	return 0
}

func (s *screen) csi(params string, final byte) {
	if strings.ContainsAny(params, "?<>=") {
		return // private modes
	}
	var ns []int
	for _, p := range strings.Split(params, ";") {
		n, _ := strconv.Atoi(p)
		ns = append(ns, n)
	}
	arg := func(i, def int) int {
		if i < len(ns) && ns[i] != 0 {
			return ns[i]
		}
		return def
	}
	switch final {
	case 'H', 'f':
		s.r, s.c = clamp(arg(0, 1)-1, 0, s.rows-1), clamp(arg(1, 1)-1, 0, s.cols-1)
	case 'A':
		s.r = clamp(s.r-arg(0, 1), 0, s.rows-1)
	case 'B':
		s.r = clamp(s.r+arg(0, 1), 0, s.rows-1)
	case 'C':
		s.c = clamp(s.c+arg(0, 1), 0, s.cols-1)
	case 'D':
		s.c = clamp(s.c-arg(0, 1), 0, s.cols-1)
	case 'G':
		s.c = clamp(arg(0, 1)-1, 0, s.cols-1)
	case 'd':
		s.r = clamp(arg(0, 1)-1, 0, s.rows-1)
	case 'X':
		for k := 0; k < arg(0, 1) && s.c+k < s.cols; k++ {
			s.cells[s.r][s.c+k] = ' '
		}
	case 'b':
		for range arg(0, 1) {
			s.put(s.last)
		}
	case 'r':
		s.top, s.bottom = clamp(arg(0, 1)-1, 0, s.rows-1), clamp(arg(1, s.rows)-1, 0, s.rows-1)
		s.r, s.c = 0, 0
	case 'S':
		s.scroll(s.top, arg(0, 1))
	case 'T':
		s.scroll(s.top, -arg(0, 1))
	case 'L':
		if s.r >= s.top && s.r <= s.bottom {
			s.scroll(s.r, -arg(0, 1))
		}
	case 'M':
		if s.r >= s.top && s.r <= s.bottom {
			s.scroll(s.r, arg(0, 1))
		}
	case 'P', '@':
		row, n := s.cells[s.r], min(arg(0, 1), s.cols-s.c)
		if final == 'P' {
			copy(row[s.c:], row[s.c+n:])
			copy(row[s.cols-n:], s.blank())
		} else {
			copy(row[s.c+n:], row[s.c:])
			copy(row[s.c:s.c+n], s.blank())
		}
	case 'K':
		from, to := s.c, s.cols
		switch arg(0, 0) {
		case 1:
			from, to = 0, s.c+1
		case 2:
			from = 0
		}
		for k := from; k < to && k < s.cols; k++ {
			s.cells[s.r][k] = ' '
		}
	case 'J':
		switch arg(0, 0) {
		case 2, 3:
			s.clear()
		case 0:
			for k := s.c; k < s.cols; k++ {
				s.cells[s.r][k] = ' '
			}
			for r := s.r + 1; r < s.rows; r++ {
				s.cells[r] = []rune(strings.Repeat(" ", s.cols))
			}
		}
	}
}

// text is the screen's non-blank lines, right-trimmed.
func (s *screen) text() string {
	var out []string
	for _, row := range s.cells {
		if line := strings.TrimRight(string(row), " "); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
