package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

// Term is the part of *terminal.Terminal the helpers need.
type Term interface {
	Lines() []string
	WriteString(string) (int, error)
	Version() uint64
}

var _ Term = (*terminal.Terminal)(nil)

// ErrScreenTimeout is bridge.ErrScreenTimeout.
var ErrScreenTimeout = bridge.ErrScreenTimeout

// WaitLines polls the screen until pred holds.
func WaitLines(ctx context.Context, t Term, timeout time.Duration, pred func([]string) bool) ([]string, error) {
	deadline := time.Now().Add(timeout)
	for {
		lines := t.Lines()
		if pred(lines) {
			return lines, nil
		}
		if time.Now().After(deadline) {
			return lines, ErrScreenTimeout
		}
		select {
		case <-ctx.Done():
			return lines, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

// Settle waits until the screen has not changed for quiet, or max elapsed.
func Settle(ctx context.Context, t Term, quiet, max time.Duration) {
	deadline := time.Now().Add(max)
	last := t.Version()
	lastChange := time.Now()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
		if v := t.Version(); v != last {
			last, lastChange = v, time.Now()
		} else if time.Since(lastChange) >= quiet {
			return
		}
	}
}

// Press sends keys one at a time with a short gap, so TUIs that debounce or
// batch input see each key separately.
func Press(ctx context.Context, t Term, keys ...string) error {
	for i, k := range keys {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(60 * time.Millisecond):
			}
		}
		if _, err := t.WriteString(k); err != nil {
			return err
		}
	}
	return nil
}

// Locator reports, for the current screen, the index of the focused row and
// of the wanted row within the same list.
type Locator func(lines []string) (focused, target int, ok bool)

// Focus moves the selection cursor with Up/Down until the wanted row is
// focused, re-reading the screen after every key instead of trusting a
// precomputed count.
func Focus(ctx context.Context, t Term, locate Locator) error {
	for step := 0; step < 40; step++ {
		lines, err := WaitLines(ctx, t, 2*time.Second, func(l []string) bool {
			_, _, ok := locate(l)
			return ok
		})
		if err != nil {
			return fmt.Errorf("locate row: %w", err)
		}
		cur, target, _ := locate(lines)
		if cur == target {
			return nil
		}
		key := terminal.KeyDown
		if target < cur {
			key = terminal.KeyUp
		}
		v := t.Version()
		if _, err := t.WriteString(key); err != nil {
			return err
		}
		_, _ = WaitLines(ctx, t, 600*time.Millisecond, func([]string) bool { return t.Version() != v })
		Settle(ctx, t, 60*time.Millisecond, 400*time.Millisecond)
	}
	return fmt.Errorf("focus row: %w", ErrScreenTimeout)
}

// Environ returns os.Environ() without the named variables, plus extra.
func Environ(drop []string, extra ...string) []string {
	dropSet := map[string]bool{}
	for _, k := range drop {
		dropSet[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !dropSet[k] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// SetEnv returns env with key set to value, replacing an existing entry.
func SetEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// HasEnv reports whether env defines key.
func HasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}
