package progress

import (
	"bytes"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// RendererOptions configures a Renderer.
type RendererOptions struct {
	// Animations and Interrupt are as in Options.
	Animations bool
	Interrupt  func()
	// Force is called, once the terminal is restored, when the user forces
	// a quit while the operation stops. It should end the process.
	Force func()
	// Input is the terminal the program reads keys from, Output the one it
	// draws on.
	Input  io.Reader
	Output io.Writer
	// NoColor draws without color (NO_COLOR).
	NoColor bool
	// LogPath returns the run's log file, shown after a failed run.
	LogPath func() string
}

type rendererState int

const (
	idle rendererState = iota
	running
	ending // waiting for the final frame
	closed
)

// Renderer is the TUI renderer: an events.Sink that draws the events as an
// inline Bubble Tea program. The program starts with the first event, so a
// command that emits nothing never takes over the terminal. A Result event,
// or Close, ends it and waits until the final frame is drawn, so whatever
// the caller prints next goes below it. After Close a later event starts a
// new program; after a Result, events are dropped.
type Renderer struct {
	opts RendererOptions

	// mu guards state. Sends hold it for reading, so ending the program
	// waits for them and no event slips in after the final frame.
	mu    sync.RWMutex
	state rendererState
	p     *tea.Program
	done  chan struct{}

	// testOpts are extra program options for tests.
	testOpts []tea.ProgramOption

	lineMu sync.Mutex
	line   []byte // Write's partial line
}

// NewRenderer returns a Renderer that hasn't started its program yet.
func NewRenderer(opts RendererOptions) *Renderer {
	return &Renderer{opts: opts}
}

// Emit draws e. A Result ends the program.
func (r *Renderer) Emit(e events.Event) {
	if res, ok := e.(events.Result); ok {
		r.end(res.OK, closed)
		return
	}
	r.mu.Lock()
	if r.state == idle {
		r.start()
	}
	r.mu.Unlock()
	r.send(EventMsg{Event: e})
}

// Close ends the program as a success if it is still running, and waits for
// it, so the caller can use the terminal. It is safe to call more than once.
func (r *Renderer) Close() { r.end(true, idle) }

// Write prints whole lines above the program while it runs, and straight to
// Output otherwise, so log records written to the same terminal don't tear
// the frame.
func (r *Renderer) Write(b []byte) (int, error) {
	r.lineMu.Lock()
	r.line = append(r.line, b...)
	var lines []string
	for {
		i := bytes.IndexByte(r.line, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(r.line[:i]))
		r.line = r.line[i+1:]
	}
	r.lineMu.Unlock()

	for _, l := range lines {
		if err := r.writeLine(l); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// writeLine prints l above a running program, or writes it to Output,
// after the final frame of a program that is ending.
func (r *Renderer) writeLine(l string) error {
	r.mu.RLock()
	state, done := r.state, r.done
	if state == running {
		r.p.Send(printMsg{text: l})
	}
	r.mu.RUnlock()
	switch state {
	case running:
		return nil
	case ending:
		<-done
	}
	_, err := io.WriteString(r.opts.Output, l+"\n")
	return err
}

// start runs the program. The caller holds mu.
func (r *Renderer) start() {
	opts := []tea.ProgramOption{
		tea.WithInput(r.opts.Input),
		tea.WithOutput(r.opts.Output),
		// The CLI owns SIGINT and SIGTERM: they cancel the command's
		// context, and the command's own exit ends the program.
		tea.WithoutSignalHandler(),
	}
	if r.opts.NoColor {
		opts = append(opts, tea.WithColorProfile(colorprofile.Ascii))
	}
	opts = append(opts, r.testOpts...)
	m := New(Options{
		Animations: r.opts.Animations,
		Interrupt:  r.opts.Interrupt,
		NoColor:    r.opts.NoColor,
		Scrollback: true,
	})
	p, done := tea.NewProgram(m, opts...), make(chan struct{})
	r.p, r.done = p, done
	r.state = running
	go func() {
		// A program that fails to start leaves the run without its
		// progress display, but the operation still runs and reports.
		final, _ := p.Run()
		close(done)
		if fm, ok := final.(Model); ok && fm.Forced && r.opts.Force != nil {
			r.opts.Force()
		}
	}()
}

// send delivers msg if the program is running, and reports whether it did.
func (r *Renderer) send(msg tea.Msg) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.state != running {
		return false
	}
	r.p.Send(msg)
	return true
}

// end tells the program the run is over, waits for its last frame, and
// moves to next: idle, so a later event starts a new program, or closed.
func (r *Renderer) end(ok bool, next rendererState) {
	r.mu.Lock()
	state, p, done := r.state, r.p, r.done
	if state == running {
		r.state = ending
	}
	r.mu.Unlock()

	switch state {
	case running:
		msg := DoneMsg{OK: ok}
		if !ok && r.opts.LogPath != nil {
			msg.LogPath = r.opts.LogPath()
		}
		p.Send(msg)
		<-done
	case ending:
		<-done
	}

	r.mu.Lock()
	if r.state != closed {
		r.state = next
	}
	r.mu.Unlock()
}
