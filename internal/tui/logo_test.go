package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestLogoRowsAreUniformWidth(t *testing.T) {
	l := newLogo()
	lines := strings.Split(l.view(), "\n")
	if len(lines) != 6 {
		t.Fatalf("logo has %d lines, want 6", len(lines))
	}
	w := lipgloss.Width(lines[0])
	if w < 40 {
		t.Fatalf("logo width %d implausibly small", w)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw != w {
			t.Errorf("line %d width = %d, want %d", i, lw, w)
		}
	}
	if logoWidth() != w {
		t.Errorf("logoWidth() = %d, want rendered width %d", logoWidth(), w)
	}
}

func TestLogoShineRendersWithoutChangingWidth(t *testing.T) {
	l := newLogo()
	l.shinePos = 10 // mid-sweep
	for i, line := range strings.Split(l.view(), "\n") {
		if w := lipgloss.Width(line); w != logoWidth() {
			t.Errorf("shining line %d width = %d, want %d", i, w, logoWidth())
		}
	}
}

func TestLogoShineDisabledWithoutAnimations(t *testing.T) {
	l := newLogo()
	if cmd := l.startShine(false); cmd != nil {
		t.Error("startShine(false) scheduled a command; want nil (static logo)")
	}
	if cmd := l.startShine(true); cmd == nil {
		t.Error("startShine(true) returned nil; want shine schedule")
	}
}

func TestLogoShineDropsAChainScheduledBeforeStop(t *testing.T) {
	l := newLogo()
	l.startShine(true)
	stale := logoShineStartMsg{seq: l.seq}
	l.stopShine()
	l.startShine(true) // back on the landing within the idle delay
	if cmd := l.update(stale); cmd != nil || l.shinePos >= 0 {
		t.Error("a start scheduled before stopShine began a second sweep chain")
	}
	if cmd := l.update(logoShineStartMsg{seq: l.seq}); cmd == nil || l.shinePos != 0 {
		t.Error("the current chain's start did not begin the sweep")
	}
	if cmd := l.update(logoShineStepMsg{seq: l.seq - 1}); cmd != nil || l.shinePos != 0 {
		t.Error("a stale step moved the sweep")
	}
}
