// Command basic is a minimal console client: type prompts, read replies,
// and answer permission prompts and questions on stdin.
//
//	go run ./examples/basic -provider claude -model haiku
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/claude"
	"github.com/jclab-joseph/clicodebridge/codex"
)

func main() {
	provider := flag.String("provider", "claude", "claude or codex")
	model := flag.String("model", "", "model id")
	mode := flag.String("mode", "", "permission mode")
	flag.Parse()

	var p bridge.Provider = claude.New(claude.Config{})
	var popts any = &claude.Options{TrustWorkspace: true}
	if *provider == "codex" {
		p, popts = codex.New(codex.Config{}), &codex.Options{TrustWorkspace: true}
	}
	ctx := context.Background()
	s, err := p.Start(ctx, bridge.SessionOptions{Model: *model, Mode: *mode, Provider: popts})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	go func() {
		for ev := range s.Events() {
			switch ev.Type {
			case bridge.EventAssistantText:
				fmt.Printf("\n%s\n", ev.Text)
			case bridge.EventToolCall:
				fmt.Printf("  [tool] %s\n", ev.Tool.Name)
			case bridge.EventTurnCompleted:
				fmt.Print("\n> ")
			case bridge.EventRequest:
				// Answered from the main goroutine to keep stdin in one place.
				pending <- *ev.Request
			case bridge.EventExited:
				fmt.Println("session ended:", ev.Text)
				os.Exit(0)
			}
		}
	}()

	fmt.Print("> ")
	lines := make(chan string)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			lines <- in.Text()
		}
		close(lines)
	}()
	for {
		select {
		case req := <-pending:
			answer(ctx, s, req, func(p string) string { fmt.Print(p); return strings.TrimSpace(<-lines) })
		case line, ok := <-lines:
			if !ok {
				return
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			if err := s.Send(ctx, line); err != nil {
				fmt.Println("send:", err)
			}
		}
	}
}

var pending = make(chan bridge.Request, 8)

func answer(ctx context.Context, s bridge.Session, req bridge.Request, ask func(string) string) {
	var resp bridge.Response
	if req.Kind == bridge.RequestPermission {
		fmt.Printf("\n%s\n  %s\n", req.Title, req.Permission.Detail)
		for i, o := range req.Permission.Options {
			fmt.Printf("  %d) %s\n", i+1, o.Label)
		}
		n, _ := strconv.Atoi(ask("choose: "))
		if n < 1 || n > len(req.Permission.Options) {
			n = len(req.Permission.Options) // last option is a denial
		}
		resp.OptionID = req.Permission.Options[n-1].ID
	} else {
		for _, q := range req.Questions {
			fmt.Printf("\n%s\n", q.Text)
			for i, o := range q.Options {
				fmt.Printf("  %d) %s\n", i+1, o.Label)
			}
			hint := "number"
			if q.MultiSelect {
				hint = "numbers separated by commas"
			}
			var a bridge.Answer
			for _, f := range strings.Split(ask(fmt.Sprintf("%s, or your own answer: ", hint)), ",") {
				f = strings.TrimSpace(f)
				if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(q.Options) {
					a.Selected = append(a.Selected, q.Options[n-1].Label)
				} else if f != "" {
					a.Text = f
				}
			}
			resp.Answers = append(resp.Answers, a)
		}
	}
	if err := s.Respond(ctx, req.ID, resp); err != nil {
		fmt.Println("respond:", err)
	}
}
