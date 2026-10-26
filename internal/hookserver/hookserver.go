// Package hookserver receives CLI hook callbacks over loopback HTTP.
//
// Claude Code supports "http" hooks: it POSTs the hook input JSON to a URL
// and treats the response body like a command hook's stdout. One server is
// shared by all sessions of a provider; each session gets an unguessable
// path prefix so other local processes cannot inject hook calls.
package hookserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Handler handles one hook call. event is the hook event name taken from the
// URL. The returned bytes are the response body; nil means "no decision".
// ctx is cancelled if the CLI gives up on the call.
type Handler func(ctx context.Context, event string, body []byte) []byte

// Server is a loopback hook endpoint.
type Server struct {
	once    sync.Once
	initErr error
	base    string
	srv     *http.Server

	mu       sync.RWMutex
	handlers map[string]Handler
}

// maxBody bounds hook payloads; tool inputs such as file writes can be large.
const maxBody = 32 << 20

func (s *Server) init() error {
	s.once.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			s.initErr = err
			return
		}
		s.handlers = map[string]Handler{}
		s.base = "http://" + ln.Addr().String()
		s.srv = &http.Server{Handler: http.HandlerFunc(s.serve)}
		go func() { _ = s.srv.Serve(ln) }()
	})
	return s.initErr
}

// Register adds a handler and returns the URL prefix to append event names
// to (prefix + "/" + event) and a function that removes the handler.
func (s *Server) Register(h Handler) (prefix string, unregister func(), err error) {
	if err := s.init(); err != nil {
		return "", nil, err
	}
	token := randomToken()
	s.mu.Lock()
	s.handlers[token] = h
	s.mu.Unlock()
	return s.base + "/h/" + token, func() {
		s.mu.Lock()
		delete(s.handlers, token)
		s.mu.Unlock()
	}, nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// Path: /h/<token>/<event>
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/h/"), "/")
	if r.Method != http.MethodPost || len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	h := s.handlers[parts[0]]
	s.mu.RUnlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out := h(r.Context(), parts[1], body)
	w.Header().Set("Content-Type", "application/json")
	if len(out) == 0 {
		out = []byte("{}")
	}
	_, _ = w.Write(out)
}

// Close stops the server.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}
