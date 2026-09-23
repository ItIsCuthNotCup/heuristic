package localfile_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore/localfile"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestStorePersistsTypedHistoryAndPagination(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	snapshot, err := store.Create(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "session-1.session.jsonl")); err != nil {
		t.Fatalf("session file: %v", err)
	}

	inputItem := llm.Item{
		ProviderID: "input-provider-id",
		Type:       llm.ItemMessage,
		Data:       llm.Message{Role: llm.RoleUser, Text: "hello"},
	}
	payload, err := json.Marshal(inputItem)
	if err != nil {
		t.Fatalf("encode input: %v", err)
	}
	input := inbox.Input{
		ID:      "input-1",
		Kind:    inbox.InputExternal,
		Payload: payload,
	}
	if err := store.AppendInput(t.Context(), snapshot.Session.ID, input); err != nil {
		t.Fatalf("append input: %v", err)
	}
	turn := session.Turn{ID: "turn-1", Type: session.TurnRegular}
	if err := store.AppendTurn(t.Context(), snapshot.Session.ID, turn); err != nil {
		t.Fatalf("append turn: %v", err)
	}

	response := sessionstore.ModelResponse{
		TurnID: turn.ID,
		Response: llm.Response{
			ID:   "response-1",
			Stop: llm.StopComplete,
			Output: []llm.Item{
				{ProviderID: "message-1", Type: llm.ItemMessage, Data: llm.Message{
					Role: llm.RoleAssistant, Text: "working", Phase: "commentary",
				}},
				{ProviderID: "call-1", Type: llm.ItemToolCall, Data: llm.ToolCall{
					CallID: "call-1", Name: "bash", Arguments: `{"command":"pwd"}`,
				}},
				{ProviderID: "reasoning-1", Type: llm.ItemReasoning, Data: llm.Reasoning{
					Summary: []string{"inspect"}, Raw: jsontext.Value(`{"encrypted":"opaque"}`),
				}},
			},
			Usage: llm.Usage{
				InputTokens: 10, OutputTokens: 4, Raw: jsontext.Value(`{"provider":14}`),
			},
		},
	}
	if err := store.AppendModelResponse(t.Context(), snapshot.Session.ID, response); err != nil {
		t.Fatalf("append model response: %v", err)
	}
	initialOperation := operation.Operation{
		ID:          "operation-1",
		Type:        "test",
		Version:     1,
		Status:      operation.StatusAwaiting,
		State:       jsontext.Value(`{"step":1}`),
		Idempotency: jsontext.Value(`{"key":"one"}`),
	}
	status := sessionstore.ToolCallStatus{
		TurnID:     turn.ID,
		CallID:     "call-1",
		Status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
		Operations: []operation.Operation{initialOperation},
	}
	if err := store.AppendToolCallStatus(t.Context(), snapshot.Session.ID, status); err != nil {
		t.Fatalf("append tool-call status: %v", err)
	}

	reopened := newStore(t, directory)
	inspected, err := reopened.Inspect(t.Context(), snapshot.Session.ID)
	if err != nil {
		t.Fatalf("inspect reopened session: %v", err)
	}
	if inspected.Session.ID != snapshot.Session.ID || !inspected.Session.CreatedAt.Equal(snapshot.Session.CreatedAt) {
		t.Fatalf("inspect = %#v, want %#v", inspected, snapshot)
	}

	first, err := reopened.Items(t.Context(), snapshot.Session.ID, sessionstore.BeforeFirst, 2)
	if err != nil {
		t.Fatalf("read first page: %v", err)
	}
	if len(first.Items) != 2 || first.NextAfter != 2 || !first.More {
		t.Fatalf("first page = %#v", first)
	}
	second, err := reopened.Items(t.Context(), snapshot.Session.ID, first.NextAfter, 2)
	if err != nil {
		t.Fatalf("read second page: %v", err)
	}
	if len(second.Items) != 2 || second.NextAfter != 4 || second.More {
		t.Fatalf("second page = %#v", second)
	}
	items := append(first.Items, second.Items...)
	wantKinds := []sessionstore.ItemKind{
		sessionstore.ItemInput,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemToolCallStatus,
	}
	for index, item := range items {
		if item.Sequence != sessionstore.Sequence(index+1) || item.Kind != wantKinds[index] {
			t.Fatalf("item %d = %#v", index, item)
		}
		if item.RecordedAt.IsZero() {
			t.Fatalf("item %d has a zero recorded time", index)
		}
	}
	gotInput := items[0].Data.(inbox.Input)
	if !reflect.DeepEqual(gotInput, input) {
		t.Fatalf("input = %#v, want %#v", gotInput, input)
	}
	gotResponse := items[2].Data.(sessionstore.ModelResponse)
	if !reflect.DeepEqual(gotResponse, response) {
		t.Fatalf("model response = %#v, want %#v", gotResponse, response)
	}
	if gotStatus := items[3].Data.(sessionstore.ToolCallStatus); !reflect.DeepEqual(gotStatus, status) {
		t.Fatalf("tool-call status = %#v, want %#v", gotStatus, status)
	}
	resume, err := reopened.Resume(t.Context(), snapshot.Session.ID)
	if err != nil {
		t.Fatalf("resume reopened session: %v", err)
	}
	if len(resume.Operations) != 1 || !reflect.DeepEqual(resume.Operations[0], initialOperation) {
		t.Fatalf("resumed operations = %#v, want %#v", resume.Operations, initialOperation)
	}
	if !reflect.DeepEqual(resume.ExternalInputIDs, []inbox.ID{"input-1"}) {
		t.Fatalf("external input IDs = %#v", resume.ExternalInputIDs)
	}

	afterEnd, err := reopened.Items(t.Context(), snapshot.Session.ID, 100, 10)
	if err != nil {
		t.Fatalf("read after end: %v", err)
	}
	if len(afterEnd.Items) != 0 || afterEnd.NextAfter != 100 || afterEnd.More {
		t.Fatalf("page after end = %#v", afterEnd)
	}
}

func TestStoreNotifiesObserversSynchronouslyAfterItemPersistence(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}

	var notifications []string
	observe := func(name string) sessionstore.Observer {
		return func(id session.ID, item sessionstore.Item) {
			reopened := newStore(t, directory)
			page, err := reopened.Items(t.Context(), id, item.Sequence-1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], item) {
				t.Fatalf("persisted items = %#v, want %#v", page.Items, item)
			}
			notifications = append(notifications, name+":"+string(item.Kind))
		}
	}

	firstID := store.AddObserver(observe("first"))
	secondID := store.AddObserver(observe("second"))
	if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: "turn-1", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	store.RemoveObserver(firstID)
	if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{
		TurnID: "turn-1", Response: llm.Response{ID: "response-1"},
	}); err != nil {
		t.Fatal(err)
	}
	store.RemoveObserver(secondID)
	if err := store.AppendInput(t.Context(), "session-1", inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"first:" + string(sessionstore.ItemTurn),
		"second:" + string(sessionstore.ItemTurn),
		"second:" + string(sessionstore.ItemModelResponse),
	}
	if !reflect.DeepEqual(notifications, want) {
		t.Fatalf("notifications = %#v, want %#v", notifications, want)
	}
}

func TestStoreObserverReceivesCanonicalToolCallStatusItem(t *testing.T) {
	store := newStore(t, t.TempDir())
	createSessionWithTurn(t, store, "session-1", "turn-1", "")

	var observed []sessionstore.Item
	store.AddObserver(func(id session.ID, item sessionstore.Item) {
		if id != "session-1" {
			t.Fatalf("session ID = %q, want session-1", id)
		}
		observed = append(observed, item)
	})
	initial := operation.Operation{
		ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	status := sessionstore.ToolCallStatus{
		TurnID:     "turn-1",
		CallID:     "call-1",
		Status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
		Operations: []operation.Operation{initial},
	}
	if err := store.AppendToolCallStatus(t.Context(), "session-1", status); err != nil {
		t.Fatal(err)
	}
	updated := initial
	updated.Status = operation.StatusAwaiting
	if err := store.SaveOperation(t.Context(), "session-1", updated); err != nil {
		t.Fatal(err)
	}

	if len(observed) != 1 {
		t.Fatalf("observed items = %#v, want one tool-call status", observed)
	}
	if observed[0].Kind != sessionstore.ItemToolCallStatus ||
		!reflect.DeepEqual(observed[0].Data, status) {
		t.Fatalf("observed item = %#v, want status %#v", observed[0], status)
	}
}

func TestStoreDoesNotNotifyObserverWhenItemPersistenceFails(t *testing.T) {
	store := newStore(t, t.TempDir())
	observed := 0
	store.AddObserver(func(session.ID, sessionstore.Item) {
		observed++
	})

	if err := store.AppendTurn(t.Context(), "missing", session.Turn{ID: "turn-1", Type: session.TurnRegular}); err == nil {
		t.Fatal("append to missing session succeeded")
	}
	if observed != 0 {
		t.Fatalf("observer called %d times after failed persistence", observed)
	}
}

func TestStoreNotifiesObserverOfForkItem(t *testing.T) {
	store := newStore(t, t.TempDir())
	createSessionWithTurn(t, store, "parent", "turn-1", "")

	var observedSession session.ID
	var observedItem sessionstore.Item
	store.AddObserver(func(id session.ID, item sessionstore.Item) {
		observedSession = id
		observedItem = item
	})
	if _, err := store.Fork(t.Context(), "child", "parent", "turn-1"); err != nil {
		t.Fatal(err)
	}

	if observedSession != "child" || observedItem.Kind != sessionstore.ItemFork {
		t.Fatalf("observed session and item = %q, %#v", observedSession, observedItem)
	}
}

func TestStoreRejectsLegacySessionResume(t *testing.T) {
	store := newStore(t, "testdata")
	_, err := store.Resume(t.Context(), "legacy-session")
	if err == nil || !strings.Contains(
		err.Error(),
		"legacy session format version 1 cannot be resumed",
	) {
		t.Fatalf("Resume error = %v", err)
	}
}

func TestStoreRejectsSoftStopHistory(t *testing.T) {
	store := newStore(t, "testdata")
	_, err := store.Resume(t.Context(), "soft-stop-session")
	if err == nil || !strings.Contains(err.Error(), `unsupported control mode "soft"`) {
		t.Fatalf("Resume error = %v, want unsupported soft-stop control", err)
	}
}

func TestStoreAppendsJSONLRecords(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "session-1.session.jsonl")
	createdInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	createTurn(t, store, "session-1", "turn-1", "")
	initial := operation.Operation{
		ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	if err := store.AppendToolCallStatus(
		t.Context(),
		"session-1",
		sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: "call-1",
			Status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
			Operations: []operation.Operation{initial},
		},
	); err != nil {
		t.Fatal(err)
	}
	updated := initial
	updated.Status = operation.StatusAwaiting
	if err := store.SaveOperation(t.Context(), "session-1", updated); err != nil {
		t.Fatal(err)
	}

	appendedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(createdInfo, appendedInfo) {
		t.Fatal("mutation replaced the session log")
	}
	appended, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(appended, created) {
		t.Fatal("mutation rewrote the committed log prefix")
	}
	lines := bytes.Split(bytes.TrimSuffix(appended, []byte{'\n'}), []byte{'\n'})
	wantTypes := []string{"session", "item", "item", "operation"}
	if len(lines) != len(wantTypes) {
		t.Fatalf("record count = %d, want %d", len(lines), len(wantTypes))
	}
	for index, wantType := range wantTypes {
		var record struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(lines[index], &record); err != nil {
			t.Fatal(err)
		}
		if record.Type != wantType {
			t.Fatalf("record %d type = %q, want %q", index, record.Type, wantType)
		}
	}
	if !bytes.Contains(lines[2], []byte(`"Operations":[`)) {
		t.Fatalf("tool status and operations are not in one record: %s", lines[2])
	}

	resume, err := newStore(t, directory).Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Operations) != 1 || resume.Operations[0].Status != operation.StatusAwaiting {
		t.Fatalf("resume = %#v", resume)
	}
}

func TestStorePersistsRepeatedToolCallStatuses(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	initial := operation.Operation{
		ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	status := sessionstore.ToolCallStatus{
		TurnID:     "turn-1",
		CallID:     "call-1",
		Status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
		Operations: []operation.Operation{initial},
	}
	if err := store.AppendToolCallStatus(t.Context(), "session-1", status); err != nil {
		t.Fatal(err)
	}
	completed := initial
	completed.Status = operation.StatusCompleted
	if err := store.SaveOperation(t.Context(), "session-1", completed); err != nil {
		t.Fatal(err)
	}
	status.Operations = []operation.Operation{completed}
	if err := store.AppendToolCallStatus(t.Context(), "session-1", status); err != nil {
		t.Fatal(err)
	}

	reopened := newStore(t, directory)
	assertStoredItemKinds(t, reopened, "session-1",
		sessionstore.ItemTurn,
		sessionstore.ItemToolCallStatus,
		sessionstore.ItemToolCallStatus,
	)
	page, err := reopened.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	first := page.Items[1].Data.(sessionstore.ToolCallStatus)
	last := page.Items[2].Data.(sessionstore.ToolCallStatus)
	if !reflect.DeepEqual(first.Operations, []operation.Operation{initial}) ||
		!reflect.DeepEqual(last.Operations, []operation.Operation{completed}) {
		t.Fatalf("status snapshots = %#v, %#v", first.Operations, last.Operations)
	}
	resume, err := reopened.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Operations) != 0 {
		t.Fatalf("resumed operations = %#v", resume.Operations)
	}
}

func TestReopenedStoreContinuesFromDerivedWriteState(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInput(t.Context(), "session-1", inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	createTurn(t, store, "session-1", "turn-1", "")
	initial := operation.Operation{
		ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	if err := store.AppendToolCallStatus(
		t.Context(),
		"session-1",
		sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: "call-1",
			Status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
			Operations: []operation.Operation{initial},
		},
	); err != nil {
		t.Fatal(err)
	}

	reopened := newStore(t, directory)
	updated := initial
	updated.Status = operation.StatusAwaiting
	if err := reopened.SaveOperation(t.Context(), "session-1", updated); err != nil {
		t.Fatal(err)
	}
	if err := reopened.AppendInput(t.Context(), "session-1", inbox.Input{
		ID: "input-2", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	createTurn(t, reopened, "session-1", "turn-2", "turn-1")

	verifier := newStore(t, directory)
	assertStoredItemKinds(t, verifier, "session-1",
		sessionstore.ItemInput,
		sessionstore.ItemTurn,
		sessionstore.ItemToolCallStatus,
		sessionstore.ItemInput,
		sessionstore.ItemTurn,
	)
	_, err := verifier.Inspect(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	resume, err := verifier.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Operations) != 1 || resume.Operations[0].Status != operation.StatusAwaiting {
		t.Fatalf("resume = %#v", resume)
	}
}

func TestKnownSessionAppendDoesNotReadLog(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "session-1.session.jsonl")
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("platform permits reading a write-only file")
	}

	if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: "turn-1", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := newStore(t, directory).Items(
		t.Context(),
		"session-1",
		sessionstore.BeforeFirst,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != sessionstore.ItemTurn {
		t.Fatalf("items = %#v", page.Items)
	}
}

func TestStoreRecoversIncompleteJSONLTailBeforeAppend(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	path := filepath.Join(directory, "session-1.session.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("incomplete-tail"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != sessionstore.ItemTurn {
		t.Fatalf("items before recovery = %#v", page.Items)
	}
	if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{
		TurnID: "turn-1", Response: llm.Response{Output: []llm.Item{}},
	}); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, []byte("incomplete-tail")) || !bytes.HasSuffix(contents, []byte{'\n'}) {
		t.Fatalf("recovered log = %q", contents)
	}
	page, err = newStore(t, directory).Items(t.Context(), "session-1", sessionstore.BeforeFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[1].Kind != sessionstore.ItemModelResponse {
		t.Fatalf("items after recovery = %#v", page.Items)
	}
}

func TestStoreRecoversFromAppendFailure(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	path := filepath.Join(directory, "session-1.session.jsonl")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	response := sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleAssistant, Text: "complete"},
		}}},
	}
	if err := store.AppendModelResponse(t.Context(), "session-1", response); err == nil {
		t.Fatal("append unexpectedly succeeded")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, committed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModelResponse(t.Context(), "session-1", response); err != nil {
		t.Fatalf("retry after append failure: %v", err)
	}

	page, err := newStore(t, directory).Items(t.Context(), "session-1", sessionstore.BeforeFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[1].Kind != sessionstore.ItemModelResponse ||
		!reflect.DeepEqual(page.Items[1].Data, response) {
		t.Fatalf("items after retry = %#v", page.Items)
	}
}

func TestCreateReplacesPopulatedSession(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "session-1", "old-turn", "")
	if err := store.AppendInput(t.Context(), "session-1", inbox.Input{
		ID: "old-input", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendToolCallStatus(
		t.Context(),
		"session-1",
		sessionstore.ToolCallStatus{
			TurnID: "old-turn",
			CallID: "old-call",
			Status: tool.CallStatus{WaitingFor: []operation.ID{"old-operation"}},
			Operations: []operation.Operation{{
				ID: "old-operation", Type: "test", Version: 1, Status: operation.StatusAwaiting,
			}},
		},
	); err != nil {
		t.Fatal(err)
	}

	replaced, err := store.Create(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	reopened := newStore(t, directory)
	got, err := reopened.Inspect(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, replaced) {
		t.Fatalf("replacement snapshot = %#v, want %#v", got, replaced)
	}
	page, err := reopened.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("replacement retained items: %#v", page.Items)
	}
	resume, err := reopened.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Operations) != 0 {
		t.Fatalf("replacement retained operations: %#v", resume.Operations)
	}
	createTurn(t, store, "session-1", "fresh-turn", "")
	assertStoredItemKinds(t, newStore(t, directory), "session-1", sessionstore.ItemTurn)
	assertNoSessionTemporaryFiles(t, directory)
}

func TestFailedReplacementEvictsCachedWriteState(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	target := filepath.Join(directory, "session-1.session.jsonl")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	blockSessionTarget(t, directory, "session-1")

	if _, err := store.Create(t.Context(), "session-1"); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	if err := os.Remove(filepath.Join(target, "sentinel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := newStore(t, directory).Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurn(t.Context(), "session-1", session.Turn{ID: "fresh-turn", Type: session.TurnRegular}); err != nil {
		t.Fatalf("append after failed replacement used stale write state: %v", err)
	}
}

func TestStoreResumesUnsettledOperationsAndSavesLatestState(t *testing.T) {
	store := newStore(t, t.TempDir())
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	operations := []operation.Operation{
		{ID: "ready", Type: "test", Version: 1, Status: operation.StatusReady, State: jsontext.Value(`{"n":1}`)},
		{ID: "awaiting", Type: "test", Version: 1, Status: operation.StatusAwaiting, State: jsontext.Value(`{"n":2}`)},
		{ID: "canceling", Type: "test", Version: 1, Status: operation.StatusCanceling, State: jsontext.Value(`{"n":3}`)},
		{ID: "complete", Type: "test", Version: 1, Status: operation.StatusCompleted, State: jsontext.Value(`{"n":4}`)},
		{ID: "failed", Type: "test", Version: 1, Status: operation.StatusFailed, State: jsontext.Value(`{"n":5}`)},
		{ID: "canceled", Type: "test", Version: 1, Status: operation.StatusCanceled, State: jsontext.Value(`{"n":6}`)},
	}
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: "call-1",
		Status: tool.CallStatus{WaitingFor: []operation.ID{
			"ready", "awaiting", "canceling", "complete", "failed", "canceled",
		}},
		Operations: operations,
	}
	if err := store.AppendToolCallStatus(t.Context(), "session-1", status); err != nil {
		t.Fatalf("append operations: %v", err)
	}

	resume, err := store.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	wantIDs := []operation.ID{"ready", "awaiting", "canceling"}
	if len(resume.Operations) != len(wantIDs) {
		t.Fatalf("resumed operations = %#v", resume.Operations)
	}
	for index, wantID := range wantIDs {
		if resume.Operations[index].ID != wantID {
			t.Fatalf("operation %d = %q, want %q", index, resume.Operations[index].ID, wantID)
		}
	}

	updated := operations[1]
	updated.Status = operation.StatusCompleted
	updated.State = jsontext.Value(`{ "n": 20 }`)
	if err := store.SaveOperation(t.Context(), "session-1", updated); err != nil {
		t.Fatalf("save operation: %v", err)
	}
	resume, err = store.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("resume after save: %v", err)
	}
	if resume.Operations[1].Status != operation.StatusCompleted || string(resume.Operations[1].State) != `{"n":20}` {
		t.Fatalf("saved terminal state = %#v, want %#v", resume.Operations[1], updated)
	}
	if len(resume.Operations) != len(wantIDs) {
		t.Fatalf("resumed operations after save = %#v", resume.Operations)
	}
	for index, wantID := range wantIDs {
		if resume.Operations[index].ID != wantID {
			t.Fatalf("operation %d after save = %q, want %q", index, resume.Operations[index].ID, wantID)
		}
	}
	operations[1] = updated
	status.Operations = operations
	if err := store.AppendToolCallStatus(t.Context(), "session-1", status); err != nil {
		t.Fatal(err)
	}
	resume, err = store.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resume.Operations, []operation.Operation{operations[0], operations[2]}) {
		t.Fatalf("recorded terminal states retained in resume: %#v", resume.Operations)
	}
	for _, index := range []int{0, 2} {
		operations[index].Status = operation.StatusCompleted
		if err := store.SaveOperation(t.Context(), "session-1", operations[index]); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendToolCallStatus(t.Context(), "session-1", status); err != nil {
		t.Fatal(err)
	}
	resume, err = store.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Operations) != 0 {
		t.Fatalf("settled session retained operations: %#v", resume.Operations)
	}
}

func TestForkCopiesHistoryThroughTurnWithoutOperations(t *testing.T) {
	store := newStore(t, t.TempDir())
	createSessionWithTurn(t, store, "parent", "turn-1", "")
	if err := store.AppendModelResponse(t.Context(), "parent", sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{ID: "response-1", Stop: llm.StopComplete, Output: []llm.Item{
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-1", Name: "test", Arguments: `{}`}},
		}},
	}); err != nil {
		t.Fatalf("append first response: %v", err)
	}
	parentOperation := operation.Operation{
		ID: "parent-operation", Type: "test", Version: 1, Status: operation.StatusAwaiting,
	}
	if err := store.AppendToolCallStatus(t.Context(), "parent", sessionstore.ToolCallStatus{
		TurnID:     "turn-1",
		CallID:     "call-1",
		Status:     tool.CallStatus{WaitingFor: []operation.ID{"parent-operation"}},
		Operations: []operation.Operation{parentOperation},
	}); err != nil {
		t.Fatalf("append parent status: %v", err)
	}
	if err := store.AppendInput(t.Context(), "parent", inbox.Input{
		ID: "next-input", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}); err != nil {
		t.Fatalf("append later input: %v", err)
	}
	createTurn(t, store, "parent", "turn-2", "turn-1")
	if err := store.AppendModelResponse(t.Context(), "parent", sessionstore.ModelResponse{
		TurnID: "turn-2", Response: llm.Response{ID: "response-2", Output: []llm.Item{}},
	}); err != nil {
		t.Fatalf("append second response: %v", err)
	}

	snapshot, err := store.Fork(t.Context(), "child", "parent", "turn-1")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if snapshot.Session.ID != "child" {
		t.Fatalf("fork snapshot = %#v", snapshot)
	}
	page, err := store.Items(t.Context(), "child", sessionstore.BeforeFirst, 100)
	if err != nil {
		t.Fatalf("read child: %v", err)
	}
	wantKinds := []sessionstore.ItemKind{
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemToolCallStatus,
		sessionstore.ItemFork,
	}
	if len(page.Items) != len(wantKinds) {
		t.Fatalf("child history = %#v", page.Items)
	}
	for index, wantKind := range wantKinds {
		if page.Items[index].Kind != wantKind {
			t.Fatalf("child item %d kind = %q, want %q", index, page.Items[index].Kind, wantKind)
		}
	}
	fork := page.Items[len(page.Items)-1].Data.(sessionstore.Fork)
	if fork.ParentID != "parent" || fork.PreviousTurnID != "turn-1" {
		t.Fatalf("fork record = %#v", fork)
	}
	resume, err := store.Resume(t.Context(), "child")
	if err != nil {
		t.Fatalf("resume child: %v", err)
	}
	if len(resume.Operations) != 0 {
		t.Fatalf("child inherited operations: %#v", resume.Operations)
	}

	if err := store.AppendInput(t.Context(), "child", inbox.Input{
		ID: "child-input", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}); err != nil {
		t.Fatalf("append child input: %v", err)
	}
	createTurn(t, store, "child", "child-turn", "turn-1")
}

func TestForkPersistsDelayedStatusBoundaries(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "parent", "turn-1", "")
	if err := store.AppendModelResponse(t.Context(), "parent", sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: llm.ToolCall{CallID: "call-1", Name: "test", Arguments: `{}`},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	createTurn(t, store, "parent", "turn-2", "turn-1")
	if err := store.AppendModelResponse(t.Context(), "parent", sessionstore.ModelResponse{
		TurnID: "turn-2", Response: llm.Response{Output: []llm.Item{}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendToolCallStatus(
		t.Context(),
		"parent",
		sessionstore.ToolCallStatus{
			TurnID: "turn-1",
			CallID: "call-1",
			Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
			Operations: []operation.Operation{{
				ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
			}},
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Fork(t.Context(), "child-1", "parent", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fork(t.Context(), "child-2", "parent", "turn-2"); err != nil {
		t.Fatal(err)
	}

	reopened := newStore(t, directory)
	assertStoredItemKinds(t, reopened, "child-1",
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemFork,
	)
	assertStoredItemKinds(t, reopened, "child-2",
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemFork,
	)
	for _, id := range []session.ID{"child-1", "child-2"} {
		resume, err := reopened.Resume(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(resume.Operations) != 0 {
			t.Fatalf("%s inherited operations: %#v", id, resume.Operations)
		}
	}
}

func TestStoreReportsMissingValuesAndCancellation(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatalf("replace session: %v", err)
	}
	if _, err := store.Inspect(t.Context(), "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing inspect error = %v, want fs.ErrNotExist", err)
	}
	if err := store.SaveOperation(t.Context(), "session-1", operation.Operation{
		ID: "missing", Type: "test", Version: 1, Status: operation.StatusReady,
	}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing operation error = %v, want fs.ErrNotExist", err)
	}
	if _, err := store.Items(t.Context(), "session-1", 0, 0); err == nil {
		t.Fatal("Items accepted a zero limit")
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Inspect(canceled, "session-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled inspect error = %v, want context.Canceled", err)
	}
}

func TestMutationsUseTheSuppliedSessionID(t *testing.T) {
	store := newStore(t, t.TempDir())
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	createSessionWithTurn(t, store, "session-2", "turn-2", "")

	if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{
		TurnID: "turn-2", Response: llm.Response{Output: []llm.Item{}},
	}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("response routed by turn ID: %v", err)
	}
	operationValue := operation.Operation{
		ID: "operation-2", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	if err := store.AppendToolCallStatus(
		t.Context(),
		"session-2",
		sessionstore.ToolCallStatus{
			TurnID: "turn-2", CallID: "call-2",
			Status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-2"}},
			Operations: []operation.Operation{operationValue},
		},
	); err != nil {
		t.Fatalf("append operation to owning session: %v", err)
	}
	operationValue.Status = operation.StatusCompleted
	if err := store.SaveOperation(t.Context(), "session-1", operationValue); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("operation routed by operation ID: %v", err)
	}
	if err := store.SaveOperation(t.Context(), "session-2", operationValue); err != nil {
		t.Fatalf("save operation in owning session: %v", err)
	}
}

func TestSeparateStoresSupportConcurrentCreates(t *testing.T) {
	directory := t.TempDir()
	first := newStore(t, directory)
	second := newStore(t, directory)
	stores := []*localfile.Store{first, second}

	const count = 24
	errorsByIndex := make([]error, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Go(func() {
			_, errorsByIndex[index] = stores[index%len(stores)].Create(
				t.Context(),
				session.ID("session-"+string(rune('a'+index))),
			)
		})
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("create %d: %v", index, err)
		}
	}
}

func TestSeparateStoresCanReplaceTheSameSession(t *testing.T) {
	directory := t.TempDir()
	stores := []*localfile.Store{newStore(t, directory), newStore(t, directory)}
	errorsByIndex := make([]error, len(stores))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Go(func() {
			<-start
			_, errorsByIndex[index] = store.Create(t.Context(), "session-1")
		})
	}
	close(start)
	wait.Wait()

	for _, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("create error = %v", err)
		}
	}
	if _, err := stores[0].Inspect(t.Context(), "session-1"); err != nil {
		t.Fatalf("inspect replaced session: %v", err)
	}
}

func TestRejectedToolCallStatusesDoNotChangePersistedState(t *testing.T) {
	tests := []struct {
		name       string
		status     tool.CallStatus
		operations []operation.Operation
	}{
		{
			name:   "invalid operation",
			status: tool.CallStatus{WaitingFor: []operation.ID{"valid", "invalid"}},
			operations: []operation.Operation{
				{ID: "valid", Type: "test", Version: 1, Status: operation.StatusReady},
				{ID: "invalid", Status: operation.StatusReady},
			},
		},
		{
			name:       "mismatched reference",
			status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-a"}},
			operations: []operation.Operation{{ID: "operation-b", Type: "test", Version: 1, Status: operation.StatusReady}},
		},
		{
			name:       "error with operation",
			status:     tool.CallStatus{Error: "invalid arguments"},
			operations: []operation.Operation{{ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store := newStore(t, directory)
			createSessionWithTurn(t, store, "session-1", "turn-1", "")
			before := sessionFileContents(t, directory)

			err := store.AppendToolCallStatus(
				t.Context(),
				"session-1",
				sessionstore.ToolCallStatus{
					TurnID:     "turn-1",
					CallID:     "call-1",
					Status:     test.status,
					Operations: test.operations,
				},
			)
			if err == nil {
				t.Fatal("invalid status accepted")
			}
			after := sessionFileContents(t, directory)
			if !bytes.Equal(after, before) {
				t.Fatal("rejected tool-call status changed the session file")
			}

			page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].Kind != sessionstore.ItemTurn {
				t.Fatalf("items after rejection = %#v", page.Items)
			}
			resume, err := store.Resume(t.Context(), "session-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(resume.Operations) != 0 {
				t.Fatalf("operations after rejection = %#v", resume.Operations)
			}
		})
	}
}

func TestMissingSessionsFailEveryReadModifyWriteMethod(t *testing.T) {
	store := newStore(t, t.TempDir())
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "items",
			call: func() error {
				_, err := store.Items(t.Context(), "missing", sessionstore.BeforeFirst, 1)
				return err
			},
		},
		{
			name: "append input",
			call: func() error {
				return store.AppendInput(t.Context(), "missing", inbox.Input{
					ID: "input-1", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
				})
			},
		},
		{
			name: "append turn",
			call: func() error {
				return store.AppendTurn(t.Context(), "missing", session.Turn{ID: "turn-1", Type: session.TurnRegular})
			},
		},
		{
			name: "append model response",
			call: func() error {
				return store.AppendModelResponse(
					t.Context(),
					"missing",
					sessionstore.ModelResponse{TurnID: "turn-1"},
				)
			},
		},
		{
			name: "append tool-call status",
			call: func() error {
				return store.AppendToolCallStatus(
					t.Context(),
					"missing",
					sessionstore.ToolCallStatus{TurnID: "turn-1", CallID: "call-1"},
				)
			},
		},
		{
			name: "save operation",
			call: func() error {
				return store.SaveOperation(t.Context(), "missing", operation.Operation{
					ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
				})
			},
		},
		{
			name: "resume",
			call: func() error {
				_, err := store.Resume(t.Context(), "missing")
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("error = %v, want fs.ErrNotExist", err)
			}
		})
	}
}

func TestRejectedPublicTransitionsDoNotChangePersistedState(t *testing.T) {
	tests := []struct {
		name string
		call func(*localfile.Store) error
	}{
		{
			name: "empty input ID",
			call: func(store *localfile.Store) error {
				return store.AppendInput(t.Context(), "session-1", inbox.Input{})
			},
		},
		{
			name: "wrong previous turn",
			call: func(store *localfile.Store) error {
				return store.AppendTurn(t.Context(), "session-1", session.Turn{
					ID: "turn-2", PreviousTurnID: "wrong",
					Type: session.TurnRegular,
				})
			},
		},
		{
			name: "invalid response usage",
			call: func(store *localfile.Store) error {
				return store.AppendModelResponse(
					t.Context(),
					"session-1",
					sessionstore.ModelResponse{
						TurnID: "turn-1",
						Response: llm.Response{
							Usage: llm.Usage{Raw: jsontext.Value(`{`)},
						},
					},
				)
			},
		},
		{
			name: "message data has wrong type",
			call: invalidResponseCall(
				t.Context(),
				llm.Item{Type: llm.ItemMessage, Data: llm.ToolCall{}},
			),
		},
		{
			name: "tool-call data has wrong type",
			call: invalidResponseCall(t.Context(), llm.Item{Type: llm.ItemToolCall, Data: llm.Message{}}),
		},
		{
			name: "tool-result data has wrong type",
			call: invalidResponseCall(t.Context(), llm.Item{Type: llm.ItemToolResult, Data: llm.Message{}}),
		},
		{
			name: "reasoning data has wrong type",
			call: invalidResponseCall(t.Context(), llm.Item{Type: llm.ItemReasoning, Data: llm.Message{}}),
		},
		{
			name: "reasoning raw data is invalid JSON",
			call: invalidResponseCall(t.Context(), llm.Item{
				Type: llm.ItemReasoning,
				Data: llm.Reasoning{Raw: jsontext.Value(`{`)},
			}),
		},
		{
			name: "unsupported response item type",
			call: invalidResponseCall(t.Context(), llm.Item{Type: "unknown", Data: struct{}{}}),
		},
		{
			name: "invalid UTF-8 in response",
			call: invalidResponseCall(t.Context(), llm.Item{
				Type: llm.ItemMessage,
				Data: llm.Message{Role: llm.RoleAssistant, Text: string([]byte{0xff})},
			}),
		},
		{
			name: "invalid operation update",
			call: func(store *localfile.Store) error {
				return store.SaveOperation(t.Context(), "session-1", operation.Operation{
					ID: "operation-1", Status: operation.StatusReady,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store := newStore(t, directory)
			createSessionWithTurn(t, store, "session-1", "turn-1", "")
			before := sessionFileContents(t, directory)
			if err := test.call(store); err == nil {
				t.Fatal("transition succeeded")
			}
			after := sessionFileContents(t, directory)
			if !bytes.Equal(after, before) {
				t.Fatal("rejected transition changed the session file")
			}
		})
	}
}

func TestStoreRejectsInvalidSessionIDsAndCancellation(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	for _, id := range []session.ID{
		"", ".", "..", "nested/session", "/absolute", "with_underscore", "with space", "unicode-ø",
		session.ID(string([]byte{0xff})),
	} {
		if _, err := store.Create(t.Context(), id); err == nil {
			t.Fatalf("invalid session ID %q accepted", id)
		}
	}
	if _, err := store.Inspect(t.Context(), ""); err == nil {
		t.Fatal("empty inspected session ID accepted")
	}
	if _, err := store.Fork(t.Context(), "", "parent", "turn-1"); err == nil {
		t.Fatal("empty fork session ID accepted")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Create(ctx, "session-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	if _, err := store.Create(t.Context(), "cached-session"); err != nil {
		t.Fatal(err)
	}
	before := sessionFileContents(t, directory)
	if err := store.AppendTurn(ctx, "cached-session", session.Turn{ID: "turn-1", Type: session.TurnRegular}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached mutation error = %v, want context.Canceled", err)
	}
	after := sessionFileContents(t, directory)
	if !bytes.Equal(after, before) {
		t.Fatal("canceled cached mutation changed the session file")
	}
}

func TestForkReportsParentTurnFailuresAndReplacesDestination(t *testing.T) {
	t.Run("self", func(t *testing.T) {
		directory := t.TempDir()
		store := newStore(t, directory)
		createSessionWithTurn(t, store, "session-1", "turn-1", "")
		before := sessionFileContents(t, directory)

		if _, err := store.Fork(t.Context(), "session-1", "session-1", "turn-1"); err == nil ||
			!strings.Contains(err.Error(), "onto itself") {
			t.Fatalf("error = %v, want self-fork rejection", err)
		}
		after := sessionFileContents(t, directory)
		if !bytes.Equal(after, before) {
			t.Fatal("self-fork rejection changed the session")
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		store := newStore(t, t.TempDir())
		if _, err := store.Fork(t.Context(), "child", "missing", "turn-1"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("missing turn", func(t *testing.T) {
		store := newStore(t, t.TempDir())
		if _, err := store.Create(t.Context(), "parent"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Fork(t.Context(), "child", "parent", "missing"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("destination exists", func(t *testing.T) {
		directory := t.TempDir()
		store := newStore(t, directory)
		createSessionWithTurn(t, store, "parent", "turn-1", "")
		createSessionWithTurn(t, store, "child", "old-child-turn", "")
		if err := store.AppendToolCallStatus(
			t.Context(),
			"child",
			sessionstore.ToolCallStatus{
				TurnID: "old-child-turn",
				CallID: "old-child-call",
				Status: tool.CallStatus{WaitingFor: []operation.ID{"old-child-operation"}},
				Operations: []operation.Operation{{
					ID: "old-child-operation", Type: "test", Version: 1, Status: operation.StatusAwaiting,
				}},
			},
		); err != nil {
			t.Fatal(err)
		}
		forked, err := store.Fork(t.Context(), "child", "parent", "turn-1")
		if err != nil {
			t.Fatal(err)
		}
		reopened := newStore(t, directory)
		got, err := reopened.Inspect(t.Context(), "child")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, forked) {
			t.Fatalf("child = %#v, want %#v", got, forked)
		}
		page, err := reopened.Items(t.Context(), "child", sessionstore.BeforeFirst, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 || page.Items[0].Kind != sessionstore.ItemTurn ||
			page.Items[0].Data.(session.Turn).ID != "turn-1" ||
			page.Items[1].Kind != sessionstore.ItemFork {
			t.Fatalf("child items = %#v", page.Items)
		}
		resume, err := reopened.Resume(t.Context(), "child")
		if err != nil {
			t.Fatal(err)
		}
		if len(resume.Operations) != 0 {
			t.Fatalf("child retained operations: %#v", resume.Operations)
		}

		if err := store.AppendInput(t.Context(), "child", inbox.Input{
			ID: "child-input", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		createTurn(t, store, "child", "child-turn", "turn-1")
		assertStoredItemKinds(
			t,
			newStore(t, directory),
			"child",
			sessionstore.ItemTurn,
			sessionstore.ItemFork,
			sessionstore.ItemInput,
			sessionstore.ItemTurn,
		)
	})
}

func TestCreateAndForkReportPublicationFailures(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		directory := t.TempDir()
		store := newStore(t, directory)
		target := blockSessionTarget(t, directory, "session-1")

		_, err := store.Create(t.Context(), "session-1")
		if err == nil || !strings.Contains(err.Error(), `create session "session-1"`) ||
			!strings.Contains(err.Error(), "publish session file") {
			t.Fatalf("error = %v, want create publication failure", err)
		}
		assertBlockedSessionTarget(t, target)
		assertNoSessionTemporaryFiles(t, directory)
	})

	t.Run("fork", func(t *testing.T) {
		directory := t.TempDir()
		store := newStore(t, directory)
		createSessionWithTurn(t, store, "parent", "turn-1", "")
		parentPath := filepath.Join(directory, "parent.session.jsonl")
		parentBefore, err := os.ReadFile(parentPath)
		if err != nil {
			t.Fatal(err)
		}
		target := blockSessionTarget(t, directory, "child")

		_, err = store.Fork(t.Context(), "child", "parent", "turn-1")
		if err == nil || !strings.Contains(err.Error(), `fork session "child"`) ||
			!strings.Contains(err.Error(), "publish session file") {
			t.Fatalf("error = %v, want fork publication failure", err)
		}
		parentAfter, readErr := os.ReadFile(parentPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(parentAfter, parentBefore) {
			t.Fatal("failed fork changed parent")
		}
		assertBlockedSessionTarget(t, target)
		assertNoSessionTemporaryFiles(t, directory)
	})
}

func TestStoreRecoversFromEncodingFailure(t *testing.T) {
	directory := t.TempDir()
	store := newStore(t, directory)
	createSessionWithTurn(t, store, "session-1", "turn-1", "")
	before := sessionFileContents(t, directory)
	err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleAssistant, Text: string([]byte{0xff})},
		}}},
	})
	if err == nil {
		t.Fatal("invalid UTF-8 response encoded")
	}
	after := sessionFileContents(t, directory)
	if !bytes.Equal(after, before) {
		t.Fatal("encoding failure changed the session file")
	}

	response := sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleAssistant, Text: "complete"},
		}}},
	}
	if err := store.AppendModelResponse(t.Context(), "session-1", response); err != nil {
		t.Fatalf("retry after encoding failure: %v", err)
	}
	page, err := newStore(t, directory).Items(t.Context(), "session-1", sessionstore.BeforeFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[1].Kind != sessionstore.ItemModelResponse ||
		!reflect.DeepEqual(page.Items[1].Data, response) {
		t.Fatalf("items after retry = %#v", page.Items)
	}
}

func newStore(t *testing.T, directory string) *localfile.Store {
	t.Helper()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

func createSessionWithTurn(
	t *testing.T,
	store *localfile.Store,
	sessionID session.ID,
	turnID session.TurnID,
	previousTurnID session.TurnID,
) {
	t.Helper()
	if _, err := store.Create(t.Context(), sessionID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createTurn(t, store, sessionID, turnID, previousTurnID)
}

func createTurn(
	t *testing.T,
	store *localfile.Store,
	sessionID session.ID,
	turnID session.TurnID,
	previousTurnID session.TurnID,
) {
	t.Helper()
	if err := store.AppendTurn(t.Context(), sessionID, session.Turn{
		ID: turnID, PreviousTurnID: previousTurnID,
		Type: session.TurnRegular,
	}); err != nil {
		t.Fatalf("append turn: %v", err)
	}
}

func sessionFileContents(t *testing.T, directory string) []byte {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".session.jsonl") {
			if path != "" {
				t.Fatal("multiple session files found")
			}
			path = filepath.Join(directory, entry.Name())
		}
	}
	if path == "" {
		t.Fatal("session file not found")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func assertStoredItemKinds(
	t *testing.T,
	store *localfile.Store,
	id session.ID,
	want ...sessionstore.ItemKind,
) {
	t.Helper()
	page, err := store.Items(t.Context(), id, sessionstore.BeforeFirst, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != len(want) {
		t.Fatalf("%s items = %#v, want kinds %v", id, page.Items, want)
	}
	for index, kind := range want {
		if page.Items[index].Kind != kind {
			t.Fatalf("%s item %d kind = %q, want %q", id, index, page.Items[index].Kind, kind)
		}
	}
}

func blockSessionTarget(t *testing.T, directory string, id session.ID) string {
	t.Helper()
	target := filepath.Join(directory, string(id)+".session.jsonl")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "sentinel"), []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertBlockedSessionTarget(t *testing.T, target string) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(target, "sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("sentinel = %q, want unchanged", contents)
	}
}

func assertNoSessionTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".session-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file remains: %q", entry.Name())
		}
	}
}

func invalidResponseCall(ctx context.Context, item llm.Item) func(*localfile.Store) error {
	return func(store *localfile.Store) error {
		return store.AppendModelResponse(ctx, "session-1", sessionstore.ModelResponse{
			TurnID:   "turn-1",
			Response: llm.Response{Output: []llm.Item{item}},
		})
	}
}
