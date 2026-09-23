package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
)

func TestRunSelectsToolsFromStartupConfiguration(t *testing.T) {
	for _, test := range []struct {
		name       string
		skill      string
		disallowed []string
		want       []string
	}{
		{name: "no integrations", want: []string{"Bash", "ViewImage"}},
		{name: "valid skill", skill: "---\nname: review\ndescription: Review code.\n---\n", want: []string{"Bash", "ViewImage", "SkillUse"}},
		{name: "disallowed SkillUse", skill: "---\nname: review\ndescription: Review code.\n---\n", disallowed: []string{"SkillUse"}, want: []string{"Bash", "ViewImage"}},
		{name: "disallowed ViewImage", disallowed: []string{"ViewImage"}, want: []string{"Bash"}},
		{name: "malformed skill", skill: "invalid", want: []string{"Bash", "ViewImage"}},
		{name: "incomplete skill", skill: "---\nname: review\n---\n", want: []string{"Bash", "ViewImage"}},
		{name: "disallowed tools", skill: "---\nname: review\ndescription: Review code.\n---\n", disallowed: []string{"Bash", "ViewImage", "SkillUse"}, want: nil},
		{name: "empty selection", disallowed: []string{"Bash", "ViewImage"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			if test.skill != "" {
				path := filepath.Join(workspace, ".harness", "skills", "review", "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(test.skill), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			input := map[string]any{"prompt": "hello", "disallowed_tools": test.disallowed}
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan llm.Request, 1)
			client := &fakeClient{respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
				requests <- request
				return llm.Response{
					ID: "response-1", Stop: llm.StopComplete,
					Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}},
				}, nil
			}}
			var stdout, stderr bytes.Buffer
			code := RunMain(t.Context(), []string{
				"-workspace", workspace, "-session-directory", t.TempDir(),
			}, func(name string) string {
				if name == "OPENAI_API_KEY" {
					return "secret"
				}
				return ""
			}, func() []string { return nil }, bytes.NewReader(encoded), &stdout, &stderr, testConfig(client))
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
			}
			request := <-requests
			var names []string
			for _, definition := range request.Tools {
				names = append(names, definition.Name)
			}
			if !slices.Equal(names, test.want) {
				t.Fatalf("advertised tools = %v, want %v", names, test.want)
			}
			system := request.Input[0].Data.(llm.Message)
			wantSkills := slices.Contains(test.want, "SkillUse")
			for _, text := range []string{
				"Use SkillUse",
				"<available_skills>",
				"<name>review</name>",
				"Review code.",
				filepath.Join(workspace, ".harness", "skills", "review", "SKILL.md"),
			} {
				if strings.Contains(system.Text, text) != wantSkills {
					t.Errorf("system prompt contains %q = %v, want %v", text, !wantSkills, wantSkills)
				}
			}
			if (test.name == "malformed skill" || test.name == "incomplete skill") && !strings.Contains(stderr.String(), "skill error>") {
				t.Fatalf("invalid skill was not reported: %q", stderr.String())
			}
		})
	}
}

func TestRunContinuesAfterUnavailableToolCall(t *testing.T) {
	for _, name := range []string{"Bash", "unknown-tool"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			sessions := t.TempDir()
			requests := make(chan llm.Request, 2)
			turn := 0
			client := &fakeClient{respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
				turn++
				if turn > 2 {
					return llm.Response{}, errors.New("unexpected extra model turn")
				}
				requests <- request
				if turn == 1 {
					return llm.Response{
						ID: "response-1", Stop: llm.StopComplete,
						Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{
							CallID: "unavailable-call", Name: name, Arguments: `{"command":"touch executed"}`,
						}}},
					}, nil
				}
				return llm.Response{
					ID: "response-2", Stop: llm.StopComplete,
					Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}},
				}, nil
			}}
			var stdout, stderr bytes.Buffer
			code := RunMain(t.Context(), []string{
				"-workspace", workspace, "-session-directory", sessions,
			}, func(name string) string {
				if name == "OPENAI_API_KEY" {
					return "secret"
				}
				return ""
			}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","disallowed_tools":["Bash"]}`), &stdout, &stderr, testConfig(client))
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
			}
			if len(requests) != 2 {
				t.Fatalf("model requests = %d, want initial and corrective turns", len(requests))
			}
			<-requests
			corrective := <-requests
			wantError := fmt.Sprintf("tool %q is not available", name)
			found := false
			for _, item := range corrective.Input {
				if item.Type == llm.ItemToolResult {
					result := item.Data.(llm.ToolResult)
					if result.CallID == "unavailable-call" && result.Output[0].Value == wantError {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("corrective request omitted tool error: %#v", corrective.Input)
			}
			statuses := 0
			for line := range strings.SplitSeq(stdout.String(), "\n") {
				if line == "" {
					continue
				}
				var item sessionstore.Item
				if err := json.Unmarshal([]byte(line), &item); err != nil {
					t.Fatal(err)
				}
				if item.Kind == sessionstore.ItemToolCallStatus {
					status := item.Data.(sessionstore.ToolCallStatus)
					if status.CallID != "unavailable-call" || status.Status.Error != wantError ||
						len(status.Status.WaitingFor) != 0 || len(status.Operations) != 0 {
						t.Fatalf("recorded tool status = %#v", status)
					}
					statuses++
				}
			}
			if statuses != 1 {
				t.Fatalf("recorded statuses = %d, want one", statuses)
			}
			if _, err := os.Stat(filepath.Join(workspace, "executed")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("excluded Bash call executed: stat error = %v", err)
			}
		})
	}
}
