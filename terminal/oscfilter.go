package terminal

// oscFilter removes OSC sequences (ESC ] ... BEL | ESC \) from a byte stream
// before it reaches the emulator, and remembers the last window title.
//
// Why: the vt emulator renders OSC payloads that contain multi-byte UTF-8
// (Claude Code writes titles like "✳ Fix tests") as visible text at the cursor
// position, which corrupts dialog rows that recognizers read. Titles are still
// useful as a status signal, so they are captured instead of dropped.
//
// The filter is streaming: a sequence split across Write calls is handled.
type oscFilter struct {
	state   oscState
	payload []byte
	title   string
}

type oscState uint8

const (
	oscNone    oscState = iota
	oscEsc              // saw ESC outside a sequence
	oscBody             // inside ESC ] ...
	oscBodyEsc          // saw ESC inside the body (possible ST)
)

// maxOSCPayload bounds memory if a sequence is never terminated.
const maxOSCPayload = 64 * 1024

// filter returns the bytes that should be forwarded to the emulator. The
// returned slice is freshly allocated.
func (f *oscFilter) filter(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		switch f.state {
		case oscNone:
			if b == 0x1b {
				f.state = oscEsc
				continue
			}
			out = append(out, b)
		case oscEsc:
			if b == ']' {
				f.state = oscBody
				f.payload = f.payload[:0]
				continue
			}
			// Not an OSC: replay the ESC we held back.
			out = append(out, 0x1b)
			if b == 0x1b {
				continue // stay in oscEsc
			}
			out = append(out, b)
			f.state = oscNone
		case oscBody:
			switch b {
			case 0x07:
				f.finish()
			case 0x1b:
				f.state = oscBodyEsc
			default:
				if len(f.payload) < maxOSCPayload {
					f.payload = append(f.payload, b)
				}
			}
		case oscBodyEsc:
			if b == '\\' {
				f.finish()
				continue
			}
			// ESC not followed by '\' aborts the OSC; treat ESC as a new start.
			f.state = oscEsc
			if b == ']' {
				f.state = oscBody
				f.payload = f.payload[:0]
				continue
			}
			out = append(out, 0x1b, b)
			f.state = oscNone
		}
	}
	return out
}

func (f *oscFilter) finish() {
	f.state = oscNone
	p := f.payload
	// OSC 0 / OSC 2 set the window title: "0;title".
	if len(p) >= 2 && (p[0] == '0' || p[0] == '2') && p[1] == ';' {
		f.title = string(p[2:])
	}
	f.payload = f.payload[:0]
}
