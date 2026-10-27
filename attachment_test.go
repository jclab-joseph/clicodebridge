package clicodebridge

import "testing"

func TestAttachmentKinds(t *testing.T) {
	for name, want := range map[string]AttachmentKind{
		"a.png": KindImage, "b.mp4": KindVideo, "c.webm": KindVideo, "d.mp3": KindAudio,
		"e.pdf": KindPDF, "f.md": KindText, "g.json": KindText, "h.bin": KindFile,
	} {
		if got := (Attachment{MediaType: MediaTypeOf(name)}).Kind(); got != want {
			t.Errorf("%s: %s (%s), want %s", name, got, MediaTypeOf(name), want)
		}
	}
	if _, err := (Attachment{}).Open(); err != ErrNoContent {
		t.Fatalf("empty attachment: %v", err)
	}
}
