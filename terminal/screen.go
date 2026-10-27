package terminal

import (
	"fmt"
	"image/color"
	"io"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// Screen is an immutable snapshot of the visible screen with per-character
// styles, for recognizers that should not depend on wording alone (a focused
// row drawn in a different color, for instance).
type Screen struct {
	// Lines holds one string per row, trailing spaces removed.
	Lines []string
	// styles[y][i] is the style of the i-th rune of Lines[y].
	styles [][]Style
}

// Style is the visual style of one character.
type Style struct {
	Fg, Bg  string // "" for the default color, else "#rrggbb"
	Bold    bool
	Faint   bool
	Italic  bool
	Reverse bool
}

// StyleAt returns the style of the i-th rune of row y.
func (s *Screen) StyleAt(y, i int) Style {
	if y < 0 || y >= len(s.styles) || i < 0 || i >= len(s.styles[y]) {
		return Style{}
	}
	return s.styles[y][i]
}

// Text joins Lines.
func (s *Screen) Text() string { return strings.Join(s.Lines, "\n") }

// PlainScreen wraps lines that carry no style information.
func PlainScreen(lines []string) *Screen { return &Screen{Lines: lines} }

func colorString(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// snapshotOf builds a Screen from an emulator. Caller holds its lock.
func snapshotOf(em interface {
	Width() int
	Height() int
	CellAt(x, y int) *uv.Cell
}) *Screen {
	w, h := em.Width(), em.Height()
	s := &Screen{Lines: make([]string, h), styles: make([][]Style, h)}
	for y := 0; y < h; y++ {
		var b strings.Builder
		var st []Style
		for x := 0; x < w; x++ {
			c := em.CellAt(x, y)
			if c == nil || c.IsZero() {
				continue // the right half of a wide character
			}
			content := c.Content
			if content == "" {
				content = " "
			}
			style := Style{
				Fg: colorString(c.Style.Fg), Bg: colorString(c.Style.Bg),
				Bold: c.Style.Attrs&uv.AttrBold != 0, Faint: c.Style.Attrs&uv.AttrFaint != 0,
				Italic: c.Style.Attrs&uv.AttrItalic != 0, Reverse: c.Style.Attrs&uv.AttrReverse != 0,
			}
			for range content {
				st = append(st, style)
			}
			b.WriteString(content)
		}
		line := strings.TrimRight(b.String(), " ")
		s.Lines[y] = line
		s.styles[y] = st[:len([]rune(line))]
	}
	// Drop trailing empty rows, like Text().
	for len(s.Lines) > 0 && s.Lines[len(s.Lines)-1] == "" {
		s.Lines, s.styles = s.Lines[:len(s.Lines)-1], s.styles[:len(s.styles)-1]
	}
	return s
}

// Screen returns a styled snapshot of the visible screen.
func (t *Terminal) Screen() *Screen {
	t.mu.Lock()
	defer t.mu.Unlock()
	return snapshotOf(t.em)
}

// Emulate replays terminal output (for example a fixture recorded from a
// Subscribe snapshot) into a fresh emulator and returns the resulting
// screen. Recognizer tests use it to see exactly what a live session sees.
func Emulate(cols, rows int, data []byte) *Screen {
	em := vt.NewEmulator(cols, rows)
	go func() { _, _ = io.Copy(io.Discard, em) }() // drain query replies
	var f oscFilter
	_, _ = em.Write(f.filter(data))
	s := snapshotOf(em)
	closeInput(em.InputPipe())
	return s
}

// closeInput ends an emulator's reply stream so the goroutine draining it
// returns. Emulator.Close is avoided: it races with a concurrent Read.
func closeInput(w io.Writer) {
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
}
