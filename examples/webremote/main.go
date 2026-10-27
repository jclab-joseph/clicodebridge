// Command webremote controls Claude Code and Codex sessions from a browser,
// similar to Claude's Remote Control: a chat view with permission and
// question dialogs, plus the live terminal of the CLI.
//
//	go run ./examples/webremote -addr 127.0.0.1:8787
//
// Sessions run with your user's full permissions. Keep the default loopback
// address, or set -token before exposing it anywhere else.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/claude"
	"github.com/jclab-joseph/clicodebridge/codex"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	token := flag.String("token", "", "require this access token (?token=… once, then a cookie)")
	claudeBin := flag.String("claude", "claude", "claude executable")
	codexBin := flag.String("codex", "codex", "codex executable")
	driftDir := flag.String("drift-dir", "", "save screens where a Claude recognizer disagrees with the CLI (fixtures for claude/testdata)")
	flag.Parse()

	claudeP := claude.New(claude.Config{Executable: *claudeBin})
	defer claudeP.Close()
	hub := newHub(*driftDir, map[string]bridge.Provider{
		"claude": claudeP,
		"codex":  codex.New(codex.Config{Executable: *codexBin}),
	})

	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/providers", hub.handleProviders)
	mux.HandleFunc("GET /api/sessions", hub.handleList)
	mux.HandleFunc("POST /api/sessions", hub.handleCreate)
	mux.HandleFunc("DELETE /api/sessions/{key}", hub.withEntry(hub.handleClose))
	mux.HandleFunc("GET /api/sessions/{key}/events", hub.withEntry(hub.handleEvents))
	mux.HandleFunc("POST /api/sessions/{key}/send", hub.withEntry(hub.handleSend))
	mux.HandleFunc("POST /api/sessions/{key}/respond", hub.withEntry(hub.handleRespond))
	mux.HandleFunc("POST /api/sessions/{key}/interrupt", hub.withEntry(hub.handleInterrupt))
	mux.HandleFunc("POST /api/sessions/{key}/command", hub.withEntry(hub.handleCommand))
	mux.HandleFunc("POST /api/sessions/{key}/settings", hub.withEntry(hub.handleSettings))
	mux.HandleFunc("GET /api/sessions/{key}/terminal", hub.withEntry(hub.handleTerminal))
	mux.HandleFunc("GET /api/sessions/{key}/file", hub.withEntry(hub.handleFile))
	mux.HandleFunc("POST /api/sessions/{key}/terminal", hub.withEntry(hub.handleTerminalInput))

	srv := &http.Server{Addr: *addr, Handler: auth(*token, mux)}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		log.Print("shutting down; closing sessions")
		hub.closeAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("webremote listening on http://%s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// auth requires the token as ?token= (which then sets a cookie, so
// EventSource requests work) or as the cookie itself.
func auth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	ok := func(v string) bool { return subtle.ConstantTimeCompare([]byte(v), []byte(token)) == 1 }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("token"); q != "" && ok(q) {
			http.SetCookie(w, &http.Cookie{Name: "webremote_token", Value: q, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("webremote_token"); err == nil && ok(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized: open this page with ?token=…", http.StatusUnauthorized)
	})
}

// --- JSON helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, bridge.ErrBusy):
		code = http.StatusConflict
	case errors.Is(err, bridge.ErrUnknownRequest):
		code = http.StatusNotFound
	case errors.Is(err, bridge.ErrExited):
		code = http.StatusGone
	}
	log.Printf("request failed (%d): %v", code, err)
	http.Error(w, err.Error(), code)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

// actionCtx bounds an action; TUI-driving calls can take a few seconds.
func actionCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 90*time.Second)
}

// --- handlers -------------------------------------------------------------------

func (h *hub) handleProviders(w http.ResponseWriter, r *http.Request) {
	var out []bridge.Info
	for _, name := range []string{"claude", "codex"} {
		info, err := h.providers[name].Info(r.Context())
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	writeJSON(w, out)
}

func (h *hub) handleList(w http.ResponseWriter, r *http.Request) { writeJSON(w, h.list()) }

type createRequest struct {
	Provider          string `json:"provider"`
	Cwd               string `json:"cwd"`
	Resume            string `json:"resume"`
	Model             string `json:"model"`
	Effort            string `json:"effort"`
	Mode              string `json:"mode"`
	RemoteControl     string `json:"remoteControl"` // "", "on", "off"
	RemoteControlName string `json:"remoteControlName"`
	Interaction       string `json:"interaction"` // "hook", "terminal"
	TrustWorkspace    bool   `json:"trustWorkspace"`
	Chrome            string `json:"chrome"` // "", "on", "off"
	Search            bool   `json:"search"`
	ScreenReader      bool   `json:"screenReader"`
	Bridge            string `json:"bridge"` // "" (auto), "off"
}

func triState(v string) *bool {
	switch v {
	case "on":
		return claude.Bool(true)
	case "off":
		return claude.Bool(false)
	}
	return nil
}

func (h *hub) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	opts := bridge.SessionOptions{Cwd: req.Cwd, Resume: req.Resume, Model: req.Model, Effort: req.Effort, Mode: req.Mode}
	switch req.Provider {
	case "claude":
		co := &claude.Options{
			RemoteControl: triState(req.RemoteControl), RemoteControlName: req.RemoteControlName,
			Chrome: triState(req.Chrome), TrustWorkspace: req.TrustWorkspace,
			ScreenReader: req.ScreenReader, DriftDir: h.driftDir,
		}
		if req.Bridge == "off" {
			co.Bridge = claude.BridgeOff
		}
		if req.Interaction == "terminal" {
			co.Interaction = claude.InteractionTerminal
		}
		opts.Provider = co
	case "codex":
		opts.Provider = &codex.Options{TrustWorkspace: req.TrustWorkspace, Search: req.Search}
	default:
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
		http.Error(w, fmt.Sprintf("working directory %q does not exist", req.Cwd), http.StatusBadRequest)
		return
	}
	ctx, cancel := actionCtx(r)
	defer cancel()
	e, err := h.start(ctx, req.Provider, opts)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, e.summary())
}

func (h *hub) handleClose(w http.ResponseWriter, r *http.Request, e *entry) {
	go e.sess.Close()
	w.WriteHeader(http.StatusAccepted)
}

func (h *hub) handleSend(w http.ResponseWriter, r *http.Request, e *entry) {
	var req struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := actionCtx(r)
	defer cancel()
	if err := e.sess.Send(ctx, req.Text); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hub) handleRespond(w http.ResponseWriter, r *http.Request, e *entry) {
	var req struct {
		RequestID string          `json:"requestId"`
		Response  bridge.Response `json:"response"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := actionCtx(r)
	defer cancel()
	if err := e.sess.Respond(ctx, req.RequestID, req.Response); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hub) handleInterrupt(w http.ResponseWriter, r *http.Request, e *entry) {
	if err := e.sess.Interrupt(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hub) handleCommand(w http.ResponseWriter, r *http.Request, e *entry) {
	var req struct {
		Command string `json:"command"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := actionCtx(r)
	defer cancel()
	if err := e.sess.Command(ctx, req.Command); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hub) handleSettings(w http.ResponseWriter, r *http.Request, e *entry) {
	var req struct {
		Model  *string `json:"model"`
		Effort *string `json:"effort"`
		Mode   *string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := actionCtx(r)
	defer cancel()
	var err error
	switch {
	case req.Mode != nil:
		err = e.sess.SetMode(ctx, *req.Mode)
	case req.Model != nil:
		err = e.sess.SetModel(ctx, *req.Model)
	case req.Effort != nil:
		err = e.sess.SetEffort(ctx, *req.Effort)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, e.sess.Settings())
}

// sse prepares a Server-Sent Events response.
func sse(w http.ResponseWriter) (func(event string, data []byte), bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	return func(event string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, strings.ReplaceAll(string(data), "\n", "\ndata: "))
		fl.Flush()
	}, true
}

// handleEvents replays the session's event history, then streams new events.
func (h *hub) handleEvents(w http.ResponseWriter, r *http.Request, e *entry) {
	send, ok := sse(w)
	if !ok {
		return
	}
	history, ch, cancel := e.subscribe()
	defer cancel()
	sum, _ := json.Marshal(e.summary())
	send("summary", sum)
	for _, ev := range history {
		b, _ := json.Marshal(ev)
		send("event", b)
	}
	send("replayed", []byte("{}"))
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			send("ping", []byte("{}"))
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			send("event", b)
			if ev.Type == bridge.EventState || ev.Type == bridge.EventSettings || ev.Type == bridge.EventSessionID {
				sum, _ := json.Marshal(e.summary())
				send("summary", sum)
			}
		}
	}
}

// handleTerminal streams the raw PTY output as base64 chunks. A "reset"
// carries a full-screen snapshot; it is sent first and again whenever the
// CLI is relaunched.
func (h *hub) handleTerminal(w http.ResponseWriter, r *http.Request, e *entry) {
	send, ok := sse(w)
	if !ok {
		return
	}
	for {
		t := e.sess.Terminal()
		snap, ch, cancel := t.Subscribe(1024)
		cols, rows := t.Size()
		meta, _ := json.Marshal(map[string]any{"cols": cols, "rows": rows, "data": base64.StdEncoding.EncodeToString(snap)})
		send("reset", meta)
	stream:
		for {
			select {
			case <-r.Context().Done():
				cancel()
				return
			case chunk, ok := <-ch:
				if !ok {
					break stream
				}
				send("data", []byte(base64.StdEncoding.EncodeToString(chunk)))
			}
		}
		cancel()
		select {
		case <-e.sess.Done():
			send("exit", []byte("{}"))
			return
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond): // relaunched, or we fell behind
		}
	}
}

// handleFile serves one attachment the session's events named (?id=…),
// inline unless ?download=1. Range requests work, so videos can seek.
func (h *hub) handleFile(w http.ResponseWriter, r *http.Request, e *entry) {
	a, ok := e.file(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var content io.ReadSeeker
	modTime := e.created
	if a.Data != nil {
		content = bytes.NewReader(a.Data)
	} else {
		f, err := os.Open(a.Path)
		if err != nil {
			http.Error(w, "the file is no longer available", http.StatusGone)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			http.Error(w, "not a regular file", http.StatusGone)
			return
		}
		content, modTime = f, fi.ModTime()
	}
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", a.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The agent chose these files: an HTML or SVG one must not run script
	// with this page's origin, which controls the sessions.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	http.ServeContent(w, r, a.Name, modTime, content)
}

func (h *hub) handleTerminalInput(w http.ResponseWriter, r *http.Request, e *entry) {
	var req struct {
		Data string `json:"data"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := e.sess.Terminal().WriteString(req.Data); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
