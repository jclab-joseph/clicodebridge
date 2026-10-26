package terminal

// Key sequences understood by TUI programs running under xterm-256color.
const (
	KeyEnter     = "\r"
	KeyEscape    = "\x1b"
	KeyTab       = "\t"
	KeyShiftTab  = "\x1b[Z"
	KeyUp        = "\x1b[A"
	KeyDown      = "\x1b[B"
	KeyRight     = "\x1b[C"
	KeyLeft      = "\x1b[D"
	KeyBackspace = "\x7f"
	KeyCtrlC     = "\x03"
	KeyCtrlD     = "\x04"
	KeyCtrlU     = "\x15" // clear line in most line editors
)

// BracketedPaste wraps text so a TUI treats it as pasted input: newlines are
// inserted literally instead of submitting the prompt.
func BracketedPaste(text string) string {
	return "\x1b[200~" + text + "\x1b[201~"
}
