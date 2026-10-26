package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sort"
	"sync"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// maxHistory bounds the per-session event replay buffer.
const maxHistory = 5000

type hub struct {
	providers map[string]bridge.Provider

	mu      sync.Mutex
	entries map[string]*entry
}

func newHub(providers map[string]bridge.Provider) *hub {
	return &hub{providers: providers, entries: map[string]*entry{}}
}

// entry is one session plus its event history and live subscribers.
type entry struct {
	key      string
	provider string
	cwd      string
	created  time.Time
	sess     bridge.Session

	mu      sync.Mutex
	history []bridge.Event
	subs    map[chan bridge.Event]struct{}
	exited  bool
}

type summary struct {
	Key      string           `json:"key"`
	Provider string           `json:"provider"`
	Cwd      string           `json:"cwd"`
	ID       string           `json:"id"`
	State    bridge.State     `json:"state"`
	Settings bridge.Settings  `json:"settings"`
	Pending  []bridge.Request `json:"pending"`
	Created  time.Time        `json:"created"`
}

func (e *entry) summary() summary {
	return summary{Key: e.key, Provider: e.provider, Cwd: e.cwd, ID: e.sess.ID(), State: e.sess.State(),
		Settings: e.sess.Settings(), Pending: e.sess.Pending(), Created: e.created}
}

func (h *hub) start(ctx context.Context, provider string, opts bridge.SessionOptions) (*entry, error) {
	s, err := h.providers[provider].Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	e := &entry{key: hex.EncodeToString(b), provider: provider, cwd: opts.Cwd, created: time.Now(), sess: s,
		subs: map[chan bridge.Event]struct{}{}}
	h.mu.Lock()
	h.entries[e.key] = e
	h.mu.Unlock()
	go e.pump()
	return e, nil
}

// pump copies session events into the history and to subscribers.
func (e *entry) pump() {
	for ev := range e.sess.Events() {
		ev.Raw = nil // provider records can be large and are not needed by the UI
		e.mu.Lock()
		e.history = append(e.history, ev)
		if len(e.history) > maxHistory {
			e.history = append([]bridge.Event(nil), e.history[len(e.history)-maxHistory:]...)
		}
		for ch := range e.subs {
			select {
			case ch <- ev:
			default: // too slow: drop it; the browser reconnects and replays
				close(ch)
				delete(e.subs, ch)
			}
		}
		e.mu.Unlock()
	}
	e.mu.Lock()
	e.exited = true
	for ch := range e.subs {
		close(ch)
		delete(e.subs, ch)
	}
	e.mu.Unlock()
}

func (e *entry) subscribe() ([]bridge.Event, <-chan bridge.Event, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	history := append([]bridge.Event(nil), e.history...)
	ch := make(chan bridge.Event, 512)
	if e.exited {
		close(ch)
		return history, ch, func() {}
	}
	e.subs[ch] = struct{}{}
	return history, ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if _, ok := e.subs[ch]; ok {
			close(ch)
			delete(e.subs, ch)
		}
	}
}

func (h *hub) list() []summary {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []summary{}
	for _, e := range h.entries {
		out = append(out, e.summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (h *hub) withEntry(f func(http.ResponseWriter, *http.Request, *entry)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		e := h.entries[r.PathValue("key")]
		h.mu.Unlock()
		if e == nil {
			http.NotFound(w, r)
			return
		}
		f(w, r, e)
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	var all []*entry
	for _, e := range h.entries {
		all = append(all, e)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range all {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.sess.Close() }()
	}
	wg.Wait()
}
