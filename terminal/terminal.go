// Package terminal runs a program on a pseudo-terminal and keeps a virtual
// screen of what it draws, so callers can both read the screen as text and
// stream the raw output to a real terminal (for example xterm.js).
package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// Options configures Start.
type Options struct {
	Path string   // executable
	Args []string // arguments, excluding the executable
	Dir  string
	Env  []string // full environment; nil means os.Environ()
	Cols int      // default 120
	Rows int      // default 40
}

// Terminal is a running program attached to a PTY.
type Terminal struct {
	cmd *exec.Cmd
	pty *os.File
	em  *vt.SafeEmulator

	writeMu sync.Mutex

	mu      sync.Mutex // guards filter, subs, nextSub, cols, rows, version
	filter  oscFilter
	subs    map[int]chan []byte
	nextSub int
	cols    int
	rows    int
	version uint64

	done    chan struct{}
	waitErr error
}

// Start launches the program.
func Start(opts Options) (*Terminal, error) {
	if opts.Cols <= 0 {
		opts.Cols = 120
	}
	if opts.Rows <= 0 {
		opts.Rows = 40
	}
	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(opts.Rows), Cols: uint16(opts.Cols)})
	if err != nil {
		return nil, fmt.Errorf("start %s on pty: %w", opts.Path, err)
	}
	t := &Terminal{
		cmd:  cmd,
		pty:  f,
		em:   vt.NewSafeEmulator(opts.Cols, opts.Rows),
		subs: map[int]chan []byte{},
		cols: opts.Cols,
		rows: opts.Rows,
		done: make(chan struct{}),
	}
	// The emulator answers terminal queries (cursor position, device
	// attributes) through its input pipe; the program blocks without them.
	go func() { _, _ = io.Copy(t.writerOnly(), t.em) }()
	go t.readLoop()
	go func() {
		t.waitErr = cmd.Wait()
		close(t.done)
	}()
	return t, nil
}

func (t *Terminal) writerOnly() io.Writer { return writerFunc(t.Write) }

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func (t *Terminal) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			t.mu.Lock()
			_, _ = t.em.Write(t.filter.filter(chunk))
			t.version++
			for id, ch := range t.subs {
				select {
				case ch <- chunk:
				default:
					// A subscriber that cannot keep up is dropped rather than
					// stalling the program; it can resubscribe for a snapshot.
					close(ch)
					delete(t.subs, id)
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			t.mu.Lock()
			for id, ch := range t.subs {
				close(ch)
				delete(t.subs, id)
			}
			t.mu.Unlock()
			return
		}
	}
}

// Write sends input bytes (keystrokes) to the program.
func (t *Terminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	select {
	case <-t.done:
		return 0, ErrExited
	default:
	}
	return t.pty.Write(p)
}

// WriteString is Write for strings.
func (t *Terminal) WriteString(s string) (int, error) { return t.Write([]byte(s)) }

// ErrExited is returned when writing to a program that has exited.
var ErrExited = errors.New("terminal: program exited")

// Text returns the visible screen as plain text, one line per row, with
// trailing spaces removed.
func (t *Terminal) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.em.String()
}

// Lines returns Text split into rows.
func (t *Terminal) Lines() []string { return strings.Split(t.Text(), "\n") }

// Version increases every time program output is processed. Callers can poll
// it cheaply to skip re-reading an unchanged screen.
func (t *Terminal) Version() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version
}

// Title returns the last window title the program set.
func (t *Terminal) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.filter.title
}

// Size returns the terminal size.
func (t *Terminal) Size() (cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// Resize changes the PTY and emulator size.
func (t *Terminal) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("terminal: invalid size %dx%d", cols, rows)
	}
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	t.em.Resize(cols, rows)
	t.mu.Unlock()
	return pty.Setsize(t.pty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Subscribe streams raw program output. The returned snapshot is an ANSI
// rendering of the current screen that, written to an empty terminal of the
// same size, reproduces it; chunks that follow continue from that state.
// The channel is closed when the program exits, when cancel is called, or if
// the subscriber falls more than buffer chunks behind.
func (t *Terminal) Subscribe(buffer int) (snapshot []byte, ch <-chan []byte, cancel func()) {
	if buffer <= 0 {
		buffer = 256
	}
	c := make(chan []byte, buffer)
	t.mu.Lock()
	snapshot = t.renderLocked()
	id := t.nextSub
	t.nextSub++
	select {
	case <-t.done:
		close(c)
	default:
		t.subs[id] = c
	}
	t.mu.Unlock()
	var once sync.Once
	return snapshot, c, func() {
		once.Do(func() {
			t.mu.Lock()
			if sc, ok := t.subs[id]; ok {
				close(sc)
				delete(t.subs, id)
			}
			t.mu.Unlock()
		})
	}
}

func (t *Terminal) renderLocked() []byte {
	var b strings.Builder
	if t.em.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString(strings.ReplaceAll(t.em.Render(), "\n", "\r\n"))
	pos := t.em.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	return []byte(b.String())
}

// Done is closed when the program exits.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Wait blocks until the program exits and returns its exit error.
func (t *Terminal) Wait() error {
	<-t.done
	return t.waitErr
}

// Pid returns the program's process id.
func (t *Terminal) Pid() int { return t.cmd.Process.Pid }

// Close kills the program if it is still running and releases the PTY.
func (t *Terminal) Close() error {
	select {
	case <-t.done:
	default:
		_ = t.cmd.Process.Kill()
		<-t.done
	}
	return t.pty.Close()
}
