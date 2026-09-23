package operation_test

import (
	"encoding/json/v2"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

func TestLocalOperationManagerLoadsEntireSkill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	want := "---\nname: review\ndescription: Review code.\n---\n\n" + strings.Repeat("instruction\n", 6_000)
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	current := newSkillUseOperation(t, "skill-use", path)
	manager := operation.NewLocalOperationManager(t.Context())
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	completed := receiveTerminalOperation(t, manager.Updates(), current.ID)
	state, err := operation.DecodeSkillUse(completed)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != operation.StatusCompleted || string(state.Content) != want || state.TerminalError != "" {
		t.Fatalf("operation = %#v, state = %#v", completed, state)
	}
}

func TestSkillUseOperationResumesPartialRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte("complete skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	current := skillUseOperationWithState(t, "skill-use-resume", operation.StatusAwaiting, operation.SkillUseState{
		Path: path, Content: []byte("complete "),
	})

	step, err := operation.AdvanceSkillUse(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := oneDispatchData[primitives.IOReadRequest](t, step, primitives.PrimitiveDispatchIORead)
	if request.Path != path || request.Offset != int64(len("complete ")) || request.Count != math.MaxInt64 {
		t.Fatalf("read request = %#v", request)
	}

	manager := operation.NewLocalOperationManager(t.Context())
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	completed := receiveTerminalOperation(t, manager.Updates(), current.ID)
	state, err := operation.DecodeSkillUse(completed)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != operation.StatusCompleted || string(state.Content) != "complete skill" {
		t.Fatalf("operation = %#v, state = %#v", completed, state)
	}
}

func TestSkillUseOperationReportsReadFailure(t *testing.T) {
	current := newSkillUseOperation(t, "skill-use-missing", filepath.Join(t.TempDir(), "missing", "SKILL.md"))
	manager := operation.NewLocalOperationManager(t.Context())
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	failed := receiveTerminalOperation(t, manager.Updates(), current.ID)
	state, err := operation.DecodeSkillUse(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "open") {
		t.Fatalf("operation = %#v, state = %#v", failed, state)
	}
}

func newSkillUseOperation(t *testing.T, id operation.ID, path string) operation.Operation {
	t.Helper()
	spec, err := operation.NewSkillUseSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		ID: id, Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State,
	}
}

func skillUseOperationWithState(
	t *testing.T,
	id operation.ID,
	status operation.Status,
	state operation.SkillUseState,
) operation.Operation {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		ID: id, Type: operation.TypeSkillUse, Version: operation.VersionSkillUse, Status: status, State: encoded,
	}
}
