// Command cbcapture records Claude Code screens as recognizer fixtures.
//
// It drives the installed claude CLI through fixed scenarios (trust dialog,
// idle, busy, permission prompt, AskUserQuestion variants) in a fresh
// directory and writes, per scenario, a styled ANSI snapshot (.ansi, what
// the recognizer tests replay) and its plain text (.txt, for review):
//
//	go run ./cmd/cbcapture -out claude/testdata/fixtures
//
// writes <out>/<cli version>/<profile>/<scenario>.{ansi,txt}. Run it after a
// CLI update, then `go test ./claude` to see which recognizers drifted. It
// spends a few cents of tokens (haiku) and leaves transcripts in ~/.claude.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jclab-joseph/clicodebridge/claude"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

const cols, rows = 120, 40

func main() {
	out := flag.String("out", "claude/testdata/fixtures", "output root")
	bin := flag.String("claude", "claude", "claude executable")
	profiles := flag.String("profiles", "classic,ax", "comma-separated: classic, ax")
	flag.Parse()

	vout, err := exec.Command(*bin, "--version").Output()
	if err != nil {
		log.Fatalf("claude --version: %v", err)
	}
	version := regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(string(vout))
	for _, p := range strings.Split(*profiles, ",") {
		dir := filepath.Join(*out, version, p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		c := &capture{bin: *bin, profile: p, dir: dir}
		if err := c.run(); err != nil {
			log.Fatalf("%s: %v", p, err)
		}
	}
	meta, _ := json.MarshalIndent(map[string]any{"version": version, "cols": cols, "rows": rows, "captured": time.Now().UTC().Format(time.RFC3339)}, "", "  ")
	_ = os.WriteFile(filepath.Join(*out, version, "meta.json"), append(meta, '\n'), 0o644)
	log.Printf("fixtures written to %s", filepath.Join(*out, version))
}

type capture struct {
	bin, profile, dir string
	t                 *terminal.Terminal
}

func (c *capture) ax() bool { return c.profile == "ax" }

// dialog is the footer line every selection dialog of the profile shows,
// so a wait does not match the prompt echo that names the same words.
func (c *capture) dialog() string {
	if c.ax() {
		return "Select with numbers"
	}
	return "Enter to select"
}

func (c *capture) save(name string) {
	snap, _, cancel := c.t.Subscribe(1)
	cancel()
	_ = os.WriteFile(filepath.Join(c.dir, name+".ansi"), snap, 0o644)
	_ = os.WriteFile(filepath.Join(c.dir, name+".txt"), []byte(c.t.Screen().Text()+"\n"), 0o644)
	log.Printf("%s/%s", c.profile, name)
}

func (c *capture) waitFor(what string, pred func(string) bool) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if pred(c.t.Screen().Text()) {
			time.Sleep(700 * time.Millisecond) // let the frame finish painting
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s; screen:\n%s", what, c.t.Screen().Text())
}

func contains(subs ...string) func(string) bool {
	return func(s string) bool {
		for _, x := range subs {
			if !strings.Contains(s, x) {
				return false
			}
		}
		return true
	}
}

// keys types each rune separately: screen-reader prompts drop fast bursts.
func (c *capture) keys(s string) {
	for _, r := range s {
		_, _ = c.t.WriteString(string(r))
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
}

func (c *capture) prompt(text string) {
	_, _ = c.t.WriteString(terminal.BracketedPaste(text))
	time.Sleep(700 * time.Millisecond)
	_, _ = c.t.WriteString("\r")
}

func (c *capture) esc() {
	_, _ = c.t.WriteString("\x1b")
	time.Sleep(2 * time.Second)
}

func (c *capture) run() error {
	work, err := os.MkdirTemp("", "cbcapture-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	args := []string{"--model", "haiku", "--permission-mode", "manual"}
	if c.ax() {
		args = append(args, "--ax-screen-reader")
	}
	c.t, err = terminal.Start(terminal.Options{Path: c.bin, Args: args, Dir: work, Cols: cols, Rows: rows,
		Env: claude.Environ("TERM=xterm-256color", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false")})
	if err != nil {
		return err
	}
	defer c.t.Close()

	steps := []func() error{
		func() error {
			if err := c.waitFor("trust dialog", contains("trust this folder")); err != nil {
				return err
			}
			c.save("trust")
			if c.ax() {
				c.keys("y\r")
			} else {
				_, _ = c.t.WriteString("\x1b[B")
				time.Sleep(500 * time.Millisecond)
				_, _ = c.t.WriteString("\r")
			}
			return nil
		},
		func() error {
			if err := c.waitFor("idle", contains("manual mode on")); err != nil {
				return err
			}
			time.Sleep(2 * time.Second)
			c.save("idle")
			return nil
		},
		func() error {
			c.prompt("Count slowly from 1 to 500, one number per line.")
			if err := c.waitFor("busy", contains("esc to interrupt")); err != nil {
				return err
			}
			time.Sleep(1500 * time.Millisecond)
			c.save("busy")
			c.esc()
			return nil
		},
		func() error {
			c.prompt("Run this exact shell command with the Bash tool: echo cap > cap.txt")
			if err := c.waitFor("permission", contains("echo cap > cap.txt", "4. No")); err != nil {
				return err
			}
			c.save("permission")
			c.esc()
			return nil
		},
		func() error {
			c.prompt(`Use the AskUserQuestion tool once with exactly two questions: (1) header "Color", question "Which color?", single select, options Red, Green, Blue. (2) header "Pets", question "Which pets?", multiSelect true, options Cat, Dog, Fish.`)
			if err := c.waitFor("question 1", contains("Which color?", c.dialog())); err != nil {
				return err
			}
			c.save("question_tabs")
			if c.ax() {
				c.keys("2\r")
			} else {
				c.keys("2")
			}
			if err := c.waitFor("question 2", contains("Which pets?", c.dialog())); err != nil {
				return err
			}
			c.save("question_multi")
			if c.ax() {
				c.keys("1,3\r")
			} else {
				c.keys("13")
				for i := 0; i < 4; i++ {
					_, _ = c.t.WriteString("\x1b[B")
					time.Sleep(300 * time.Millisecond)
				}
				time.Sleep(500 * time.Millisecond)
				c.save("question_multi_submit")
				_, _ = c.t.WriteString("\r")
			}
			if err := c.waitFor("review", contains("Ready to submit")); err != nil {
				return err
			}
			c.save("review")
			c.esc()
			c.esc()
			return nil
		},
		func() error {
			c.prompt(`Use the AskUserQuestion tool once with one question: header "Drink", question "Which drink?", single select, options Tea, Coffee.`)
			if err := c.waitFor("single question", contains("Which drink?", c.dialog())); err != nil {
				return err
			}
			c.save("question_single")
			if c.ax() {
				c.keys("3\r")
				if err := c.waitFor("text prompt", contains("Enter text")); err != nil {
					return err
				}
				c.save("question_text_prompt")
			} else {
				_, _ = c.t.WriteString("\x1b[B")
				time.Sleep(300 * time.Millisecond)
				_, _ = c.t.WriteString("\x1b[B")
				time.Sleep(300 * time.Millisecond)
				c.keys("Water")
				if err := c.waitFor("typed answer", contains("Water", c.dialog())); err != nil {
					return err
				}
				c.save("question_text_typed")
			}
			c.esc()
			c.esc()
			return nil
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}
