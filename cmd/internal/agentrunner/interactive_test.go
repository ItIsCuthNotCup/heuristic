package agentrunner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func interactiveConfig(client Client) Config {
	config := testConfig(client)
	config.Interactive = true
	return config
}

func TestInteractiveRunTwoPromptsThenQuit(t *testing.T) {
	var mu sync.Mutex
	var replies []string
	client := &fakeClient{
		respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
			var last string
			for _, item := range request.Input {
				if msg, ok := item.Data.(llm.Message); ok && msg.Role == llm.RoleUser {
					last = msg.Text
				}
			}
			reply := "reply to " + last
			mu.Lock()
			replies = append(replies, reply)
			mu.Unlock()
			return llm.Response{
				ID:   "r",
				Stop: llm.StopComplete,
				Output: []llm.Item{{
					Type: llm.ItemMessage,
					Data: llm.Message{Role: llm.RoleAssistant, Text: reply},
				}},
			}, nil
		},
	}
	workspace := t.TempDir()
	sessions := t.TempDir()
	var stdout, stderr bytes.Buffer
	var outMu sync.Mutex
	outWriter := &lockedWriter{w: &stdout, mu: &outMu}
	pipeR, pipeW := io.Pipe()
	// Feed the second question only after the first reply rendered, so the
	// two inputs land in separate turns.
	go func() {
		pipeW.Write([]byte("first question\n"))
		for range 200 {
			outMu.Lock()
			done := strings.Contains(stdout.String(), "reply to first question")
			outMu.Unlock()
			if done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		pipeW.Write([]byte("second question\n"))
		for range 200 {
			outMu.Lock()
			done := strings.Contains(stdout.String(), "reply to second question")
			outMu.Unlock()
			if done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		pipeW.Write([]byte("/quit\n"))
	}()
	code := RunMain(
		t.Context(),
		[]string{"-workspace", workspace, "-session-directory", sessions},
		func(name string) string {
			switch name {
			case "OPENAI_API_KEY":
				return "secret"
			case "SHELL":
				return "/bin/sh"
			default:
				return ""
			}
		},
		func() []string { return []string{"PATH=/usr/bin:/bin"} },
		pipeR,
		outWriter,
		&stderr,
		interactiveConfig(client),
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Heuristic — an agent that doesn't overthink") {
		t.Fatalf("missing banner: %q", out)
	}
	if !strings.Contains(out, "reply to first question") || !strings.Contains(out, "reply to second question") {
		t.Fatalf("missing replies: %q", out)
	}
	if strings.Contains(out, `"kind":"model_response"`) || strings.Contains(out, `"type":"model_response"`) {
		t.Fatalf("JSONL leaked to output: %q", out)
	}
	if strings.Count(out, "› ") != 3 {
		t.Fatalf("prompt count = %q, want banner prompt plus one after each reply", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 2 {
		t.Fatalf("replies = %v", replies)
	}
}

func TestInteractiveDashPRunsAndExits(t *testing.T) {
	client := &fakeClient{
		respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
			return llm.Response{
				ID:   "r",
				Stop: llm.StopComplete,
				Output: []llm.Item{{
					Type: llm.ItemMessage,
					Data: llm.Message{Role: llm.RoleAssistant, Text: "one shot done"},
				}},
			}, nil
		},
	}
	workspace := t.TempDir()
	sessions := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := RunMain(
		t.Context(),
		[]string{"-workspace", workspace, "-session-directory", sessions, "-p", "do it"},
		func(name string) string {
			if name == "OPENAI_API_KEY" {
				return "secret"
			}
			return ""
		},
		func() []string { return []string{"PATH=/usr/bin:/bin"} },
		strings.NewReader(""), // stdin empty: must not block
		&stdout,
		&stderr,
		interactiveConfig(client),
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "one shot done") {
		t.Fatalf("missing reply: %q", stdout.String())
	}
}

func TestInteractivePositionalArgsJoinAsPrompt(t *testing.T) {
	seen := make(chan string, 1)
	client := &fakeClient{
		respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
			for _, item := range request.Input {
				if msg, ok := item.Data.(llm.Message); ok && msg.Role == llm.RoleUser {
					seen <- msg.Text
				}
			}
			return llm.Response{
				ID:   "r",
				Stop: llm.StopComplete,
				Output: []llm.Item{{
					Type: llm.ItemMessage,
					Data: llm.Message{Role: llm.RoleAssistant, Text: "ok"},
				}},
			}, nil
		},
	}
	workspace := t.TempDir()
	sessions := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := RunMain(
		t.Context(),
		[]string{"-workspace", workspace, "-session-directory", sessions, "fix", "the", "failing", "test"},
		func(name string) string {
			if name == "OPENAI_API_KEY" {
				return "secret"
			}
			return ""
		},
		func() []string { return []string{"PATH=/usr/bin:/bin"} },
		strings.NewReader(""), // EOF → StopWhenIdle
		&stdout,
		&stderr,
		interactiveConfig(client),
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if got := <-seen; got != "fix the failing test" {
		t.Fatalf("prompt = %q, want joined positional args", got)
	}
}

func TestInteractiveRejectsResumeInOneShot(t *testing.T) {
	var stderr bytes.Buffer
	code := RunMain(
		t.Context(),
		[]string{"-resume", "abc"},
		func(string) string { return "" },
		func() []string { return nil },
		strings.NewReader(`{"prompt":"x"}`),
		&bytes.Buffer{},
		&stderr,
		testConfig(&fakeClient{respond: func(context.Context, llm.Request) (llm.Response, error) {
			return llm.Response{}, errors.New("unreachable")
		}}),
	)
	if code != 1 || !strings.Contains(stderr.String(), "-resume requires interactive mode") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
