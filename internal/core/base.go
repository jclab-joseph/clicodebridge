// Package core holds session bookkeeping shared by providers: event
// delivery, state, settings and pending requests.
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/internal/eventq"
)

// Pending is a request plus provider-private data needed to answer it.
type Pending struct {
	Req     bridge.Request
	Data    any
	Emitted bool
}

// Base implements the bookkeeping half of bridge.Session.
type Base struct {
	provider string
	q        *eventq.Queue[bridge.Event]

	mu       sync.Mutex
	id       string
	state    bridge.State
	settings bridge.Settings
	pending  []*Pending
	changed  chan struct{}
	done     chan struct{}
	finished bool
}

// NewBase returns a Base in StateStarting.
func NewBase(provider string) *Base {
	return &Base{
		provider: provider,
		q:        eventq.New[bridge.Event](),
		state:    bridge.StateStarting,
		changed:  make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (b *Base) Provider() string            { return b.provider }
func (b *Base) Events() <-chan bridge.Event { return b.q.Out() }
func (b *Base) Done() <-chan struct{}       { return b.done }
func (b *Base) Lock()                       { b.mu.Lock() }
func (b *Base) Unlock()                     { b.mu.Unlock() }
func (b *Base) notifyLocked()               { close(b.changed); b.changed = make(chan struct{}) }
func (b *Base) emitLocked(ev bridge.Event)  { ev.Time = time.Now(); ev.SessionID = b.id; b.q.Push(ev) }
func (b *Base) Emit(ev bridge.Event)        { b.mu.Lock(); b.emitLocked(ev); b.mu.Unlock() }
func (b *Base) Errorf(err error)            { b.Emit(bridge.Event{Type: bridge.EventError, Text: err.Error()}) }
func (b *Base) ID() string                  { b.mu.Lock(); defer b.mu.Unlock(); return b.id }
func (b *Base) State() bridge.State         { b.mu.Lock(); defer b.mu.Unlock(); return b.state }
func (b *Base) Settings() bridge.Settings   { b.mu.Lock(); defer b.mu.Unlock(); return b.settings }

// SetID records the CLI session id and emits EventSessionID when it changes.
func (b *Base) SetID(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" || id == b.id {
		return
	}
	b.id = id
	b.emitLocked(bridge.Event{Type: bridge.EventSessionID})
	b.notifyLocked()
}

// SetState emits EventState when the state changes. Exited is final.
func (b *Base) SetState(s bridge.State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s == b.state || b.state == bridge.StateExited {
		return
	}
	b.state = s
	b.emitLocked(bridge.Event{Type: bridge.EventState, State: s})
	b.notifyLocked()
}

// UpdateSettings applies f and emits EventSettings if anything changed.
func (b *Base) UpdateSettings(f func(*bridge.Settings)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := b.settings
	f(&next)
	if next == b.settings {
		return
	}
	b.settings = next
	s := next
	b.emitLocked(bridge.Event{Type: bridge.EventSettings, Settings: &s})
	b.notifyLocked()
}

// AddPending registers a request. If emit is true EventRequest is sent now;
// otherwise call EmitPending once the request is complete.
func (b *Base) AddPending(p *Pending, emit bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.Req.ID == "" {
		p.Req.ID = NewID()
	}
	if p.Req.Time.IsZero() {
		p.Req.Time = time.Now()
	}
	b.pending = append(b.pending, p)
	if emit {
		b.emitPendingLocked(p)
	}
	b.notifyLocked()
}

// EmitPending sends EventRequest for a request added with emit=false.
func (b *Base) EmitPending(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pending {
		if p.Req.ID == id && !p.Emitted {
			b.emitPendingLocked(p)
			b.notifyLocked()
		}
	}
}

func (b *Base) emitPendingLocked(p *Pending) {
	p.Emitted = true
	r := p.Req
	b.emitLocked(bridge.Event{Type: bridge.EventRequest, Request: &r})
}

// FindPending returns the pending request with id, or nil.
func (b *Base) FindPending(id string) *Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pending {
		if p.Req.ID == id {
			return p
		}
	}
	return nil
}

// PendingWhere returns pending entries matching f.
func (b *Base) PendingWhere(f func(*Pending) bool) []*Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*Pending
	for _, p := range b.pending {
		if f(p) {
			out = append(out, p)
		}
	}
	return out
}

// HasPending reports whether any request is pending.
func (b *Base) HasPending() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending) > 0
}

// Pending lists emitted pending requests.
func (b *Base) Pending() []bridge.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []bridge.Request
	for _, p := range b.pending {
		if p.Emitted {
			out = append(out, p.Req)
		}
	}
	return out
}

// Resolve removes a pending request and emits EventRequestResolved if it had
// been emitted. It reports whether the request was pending.
func (b *Base) Resolve(id string, how bridge.Resolution) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, p := range b.pending {
		if p.Req.ID == id {
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			if p.Emitted {
				b.emitLocked(bridge.Event{Type: bridge.EventRequestResolved, RequestID: id, Resolution: how})
			}
			b.notifyLocked()
			return true
		}
	}
	return false
}

// ResolveAll resolves every pending request.
func (b *Base) ResolveAll(how bridge.Resolution) {
	for _, p := range b.PendingWhere(func(*Pending) bool { return true }) {
		b.Resolve(p.Req.ID, how)
	}
}

// WaitFor blocks until cond returns true, re-checking on every state,
// settings or pending change and at least every interval.
func (b *Base) WaitFor(ctx context.Context, interval time.Duration, cond func() bool) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		b.mu.Lock()
		ch := b.changed
		b.mu.Unlock()
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.done:
			if cond() {
				return nil
			}
			return bridge.ErrExited
		case <-ch:
		case <-t.C:
		}
	}
}

// Finish emits EventExited, closes the event stream and Done. Idempotent.
func (b *Base) Finish(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	b.finished = true
	for _, p := range b.pending {
		if p.Emitted {
			b.emitLocked(bridge.Event{Type: bridge.EventRequestResolved, RequestID: p.Req.ID, Resolution: bridge.ResolvedCancelled})
		}
	}
	b.pending = nil
	if b.state != bridge.StateExited {
		b.state = bridge.StateExited
		b.emitLocked(bridge.Event{Type: bridge.EventState, State: bridge.StateExited})
	}
	b.emitLocked(bridge.Event{Type: bridge.EventExited, Text: reason})
	b.q.Close()
	b.notifyLocked()
	close(b.done)
}

// NewID returns a random request id.
func NewID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
