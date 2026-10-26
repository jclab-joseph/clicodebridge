package terminal

import (
	"strings"
	"testing"
	"time"
)

func TestOSCFilter(t *testing.T) {
	var f oscFilter
	in := "\x1b[3;1HABC\x1b]0;\xe2\x9c\xb3 Ti" // split mid-sequence
	out := string(f.filter([]byte(in)))
	out += string(f.filter([]byte("tle\x07X\x1b]2;Other\x1b\\Y\x1b[0m")))
	if out != "\x1b[3;1HABCXY\x1b[0m" {
		t.Fatalf("filtered = %q", out)
	}
	if f.title != "Other" {
		t.Fatalf("title = %q", f.title)
	}
	f.filter([]byte("\x1b]0;\xe2\x9c\xb3 Fix\x07"))
	if f.title != "✳ Fix" {
		t.Fatalf("title = %q", f.title)
	}
}

func waitText(t *term, want string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(t.Text(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

type term = Terminal

func TestTerminalRoundTrip(t *testing.T) {
	tm, err := Start(Options{Path: "sh", Args: []string{"-c", `printf '\033]0;\342\234\263 busy\007ready\n'; read x; printf "got:%s\n" "$x"`}, Cols: 40, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	if !waitText(tm, "ready") {
		t.Fatalf("screen = %q", tm.Text())
	}
	if tm.Title() != "✳ busy" || strings.Contains(tm.Text(), "busy") {
		t.Fatalf("title %q leaked or missing; screen %q", tm.Title(), tm.Text())
	}
	snap, ch, cancel := tm.Subscribe(16)
	defer cancel()
	if !strings.Contains(string(snap), "ready") {
		t.Fatalf("snapshot = %q", snap)
	}
	if _, err := tm.WriteString("hello\r"); err != nil {
		t.Fatal(err)
	}
	if !waitText(tm, "got:hello") {
		t.Fatalf("screen = %q", tm.Text())
	}
	var raw strings.Builder
	for chunk := range ch { // closed when the program exits
		raw.Write(chunk)
	}
	if !strings.Contains(raw.String(), "got:hello") {
		t.Fatalf("raw stream = %q", raw.String())
	}
	if err := tm.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if _, err := tm.WriteString("x"); err != ErrExited {
		t.Fatalf("write after exit: %v", err)
	}
}
