package clicodebridge

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// Attachment is a file the agent showed or sent: an image a tool returned
// (a screenshot, a Read of a picture), or a file the agent delivered to the
// user on purpose (Claude's SendUserFile, the bridge mod's send_file).
//
// Content is either inline (Data, decoded from the transcript) or on disk
// (Path, as the agent named it). Use Open to read either.
type Attachment struct {
	// ID is unique within the session.
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size,omitempty"`
	// Path is the file on disk for a sent file; empty for inline content.
	Path string `json:"path,omitempty"`
	// Data is the inline content; never serialized.
	Data []byte `json:"-"`
	// Sent is true for files the agent delivered to the user, false for
	// content that merely appeared in a tool result or prompt.
	Sent    bool   `json:"sent,omitempty"`
	Caption string `json:"caption,omitempty"`
}

// AttachmentKind is a coarse category for presentation.
type AttachmentKind string

const (
	KindImage AttachmentKind = "image"
	KindVideo AttachmentKind = "video"
	KindAudio AttachmentKind = "audio"
	KindPDF   AttachmentKind = "pdf"
	KindText  AttachmentKind = "text"
	KindFile  AttachmentKind = "file"
)

// Kind categorizes the attachment by media type.
func (a Attachment) Kind() AttachmentKind {
	mt := a.MediaType
	switch {
	case strings.HasPrefix(mt, "image/"):
		return KindImage
	case strings.HasPrefix(mt, "video/"):
		return KindVideo
	case strings.HasPrefix(mt, "audio/"):
		return KindAudio
	case mt == "application/pdf":
		return KindPDF
	case strings.HasPrefix(mt, "text/"), mt == "application/json":
		return KindText
	}
	return KindFile
}

// ErrNoContent is returned by Open for an attachment with neither data nor a
// path.
var ErrNoContent = errors.New("clicodebridge: attachment has no content")

// Open returns the attachment's content. A sent file is read from disk as it
// is now, which may differ from when it was sent.
func (a Attachment) Open() (io.ReadCloser, error) {
	if a.Data != nil {
		return io.NopCloser(bytes.NewReader(a.Data)), nil
	}
	if a.Path == "" {
		return nil, ErrNoContent
	}
	return os.Open(a.Path)
}

// MediaTypeOf guesses a media type from a file name.
func MediaTypeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".log", ".txt":
		return "text/plain"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if i := strings.IndexByte(t, ';'); i > 0 {
			t = t[:i]
		}
		return t
	}
	return "application/octet-stream"
}

// FileAttachment describes a file on disk.
func FileAttachment(id, path, caption string) Attachment {
	a := Attachment{ID: id, Name: filepath.Base(path), Path: path, MediaType: MediaTypeOf(path), Sent: true, Caption: caption}
	if fi, err := os.Stat(path); err == nil {
		a.Size = fi.Size()
	}
	return a
}
