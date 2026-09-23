package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
)

func TestSkillUseLoadsRegisteredSkillByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	want := "---\nname: review\ndescription: Review code.\n---\n\nRead all instructions.\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(StaticTranslators{}, SkillUseName)
	if _, err := registry.RegisterSkill(Skill{Name: "review", Description: "Review code.", Path: path}); err != nil {
		t.Fatal(err)
	}
	translator, exists := registry.Resolve(SkillUseName)
	if !exists {
		t.Fatal("SkillUse is not registered")
	}
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{Name: SkillUseName, Arguments: `{"name":"review"}`})
	if status.Error != "" || len(ctx.specs) != 1 ||
		len(status.WaitingFor) != 1 || status.WaitingFor[0] != "operation-1" {
		t.Fatalf("status = %#v, specs = %#v", status, ctx.specs)
	}

	current := operation.Operation{
		ID: "operation-1", Type: ctx.specs[0].Type, Version: ctx.specs[0].Version,
		Status: operation.StatusReady, State: ctx.specs[0].State,
	}
	manager := operation.NewLocalOperationManager(t.Context())
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	completed := receiveSkillUseOperation(t, manager.Updates(), current.ID)
	result, err := translator.TranslateResult("call-1", status, []operation.Operation{completed})
	if err != nil {
		t.Fatal(err)
	}
	if result.CallID != "call-1" || result.Output[0].Value != want {
		t.Fatalf("result = %#v", result)
	}
}

func TestSkillUseRejectsInvalidSelection(t *testing.T) {
	registry := NewRegistry(StaticTranslators{}, SkillUseName)
	translator, exists := registry.Resolve(SkillUseName)
	if !exists {
		t.Fatal("SkillUse is not registered")
	}
	for _, test := range []struct {
		name      string
		call      llm.ToolCall
		wantError string
	}{
		{name: "wrong tool", call: llm.ToolCall{Name: "Other"}, wantError: "does not match"},
		{name: "invalid JSON", call: llm.ToolCall{Arguments: `{`}, wantError: "decode skill-use arguments"},
		{name: "missing name", call: llm.ToolCall{Arguments: `{}`}, wantError: `argument "name" must be set`},
		{name: "unknown skill", call: llm.ToolCall{Arguments: `{"name":"missing"}`}, wantError: `skill "missing" is not registered`},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := translator.Translate(&recordingContext{}, test.call)
			if !strings.Contains(status.Error, test.wantError) || len(status.WaitingFor) != 0 {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func receiveSkillUseOperation(
	t *testing.T,
	updates <-chan operation.Operation,
	id operation.ID,
) operation.Operation {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case current := <-updates:
			if current.ID == id && (current.Status == operation.StatusCompleted ||
				current.Status == operation.StatusFailed || current.Status == operation.StatusCanceled) {
				return current
			}
		case <-timer.C:
			t.Fatal("timed out waiting for skill-use operation")
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
}
