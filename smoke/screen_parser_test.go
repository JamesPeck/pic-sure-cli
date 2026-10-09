package smoke

import "testing"

// The screen must render the same however the PTY splits its output
// between reads, including mid-sequence and mid-rune.
func TestScreenSplitInput(t *testing.T) {
	out := []byte("hello\x1b[2Jworld\x1b]0;title\x07 é✓\x1b]2;t\x1b\\\x1b(B\x1b[1;1Hx")
	const want = "x    world é✓"

	whole := newScreen(2, 20)
	whole.write(out)
	if got := whole.text(); got != want {
		t.Fatalf("one write: %q, want %q", got, want)
	}
	for at := 1; at < len(out); at++ {
		s := newScreen(2, 20)
		s.write(out[:at])
		s.write(out[at:])
		if got := s.text(); got != want {
			t.Errorf("split at %d: %q, want %q", at, got, want)
		}
	}
	bytewise := newScreen(2, 20)
	for i := range out {
		bytewise.write(out[i : i+1])
	}
	if got := bytewise.text(); got != want {
		t.Errorf("a byte at a time: %q, want %q", got, want)
	}
}
