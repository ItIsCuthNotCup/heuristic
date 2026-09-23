package inbox_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
)

func TestInboxOutputsNewInputs(t *testing.T) {
	inputs := newInbox(t)
	first := inbox.Input{
		ID:      "input-1",
		Kind:    inbox.InputExternal,
		Payload: jsontext.Value(`{"message":"hello"}`),
	}
	if got := submitAndReceive(t, inputs, first); !reflect.DeepEqual(got, first) {
		t.Fatalf("output = %#v, want %#v", got, first)
	}
}

func TestInboxOutputsEveryInputKind(t *testing.T) {
	inputs := newInbox(t)
	want := []inbox.Input{
		{ID: "external", Kind: inbox.InputExternal},
		{ID: "control", Kind: inbox.InputControl, Payload: jsontext.Value(`{"Mode":"hard"}`)},
		{ID: "crash", Kind: inbox.InputCrash},
	}
	for _, input := range want {
		if got := submitAndReceive(t, inputs, input); !reflect.DeepEqual(got, input) {
			t.Fatalf("output = %#v, want %#v", got, input)
		}
	}
}

func TestInboxDeduplicatesInputID(t *testing.T) {
	inputs := newInbox(t)
	first := inbox.Input{
		ID:      "same-input",
		Kind:    inbox.InputExternal,
		Payload: jsontext.Value(`{"message":"first"}`),
	}
	duplicate := inbox.Input{
		ID:      "same-input",
		Kind:    inbox.InputControl,
		Payload: jsontext.Value(`{"Mode":"hard","Reason":"different"}`),
	}
	second := testInput("next-input")
	if got := submitAndReceive(t, inputs, first); !reflect.DeepEqual(got, first) {
		t.Fatalf("first output = %#v, want %#v", got, first)
	}
	if err := inputs.Submit(t.Context(), duplicate); err != nil {
		t.Fatal(err)
	}
	if got := submitAndReceive(t, inputs, second); !reflect.DeepEqual(got, second) {
		t.Fatalf("second output = %#v, want %#v", got, second)
	}
	assertNoInput(t, inputs.Output())
}

func TestInboxDeduplicatesQueuedInputID(t *testing.T) {
	inputs := newInbox(t)
	first := inbox.Input{
		ID: "same-input", Kind: inbox.InputExternal,
		Payload: jsontext.Value(`{"value":"first"}`),
	}
	duplicate := inbox.Input{
		ID: "same-input", Kind: inbox.InputExternal,
		Payload: jsontext.Value(`{"value":"duplicate"}`),
	}
	if err := inputs.Submit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := inputs.Submit(t.Context(), duplicate); err != nil {
		t.Fatal(err)
	}

	if got := receiveInput(t, inputs.Output()); !reflect.DeepEqual(got, first) {
		t.Fatalf("output = %#v, want %#v", got, first)
	}
	assertNoInput(t, inputs.Output())
}

func TestInboxDeduplicatesConcurrentSubmissions(t *testing.T) {
	inputs := newInbox(t)
	const count = 100
	errors := make(chan error, count)
	var group sync.WaitGroup
	for range count {
		group.Go(func() {
			errors <- inputs.Submit(t.Context(), testInput("same-input"))
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := receiveInput(t, inputs.Output()); got.ID != "same-input" {
		t.Fatalf("output ID = %q, want same-input", got.ID)
	}
	assertNoInput(t, inputs.Output())
}

func TestInboxKeepsDifferentIDsDistinct(t *testing.T) {
	inputs := newInbox(t)
	payload := jsontext.Value(`{"message":"same"}`)
	first := inbox.Input{ID: "first", Kind: inbox.InputExternal, Payload: payload}
	second := inbox.Input{ID: "second", Kind: inbox.InputExternal, Payload: payload}
	for _, input := range []inbox.Input{first, second} {
		if got := submitAndReceive(t, inputs, input); !reflect.DeepEqual(got, input) {
			t.Fatalf("output = %#v, want %#v", got, input)
		}
	}
}

func TestInboxAcceptsConcurrentSubmissions(t *testing.T) {
	inputs := newInbox(t)
	const count = 100
	errors := make(chan error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			errors <- inputs.Submit(t.Context(), testInput(inbox.ID(strconv.Itoa(index))))
		})
	}
	ids := make([]int, 0, count)
	for range count {
		value, err := strconv.Atoi(string(receiveInput(t, inputs.Output()).ID))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, value)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Ints(ids)
	for index, id := range ids {
		if id != index {
			t.Fatalf("ID[%d] = %d, want %d", index, id, index)
		}
	}
}

func TestInboxSubmitDoesNotWaitForOutputConsumer(t *testing.T) {
	inputs := newInbox(t)
	for index := range 100 {
		if err := inputs.Submit(t.Context(), testInput(inbox.ID(strconv.Itoa(index)))); err != nil {
			t.Fatalf("submit %d: %v", index, err)
		}
	}
	for index := range 100 {
		if got := receiveInput(t, inputs.Output()).ID; got != inbox.ID(strconv.Itoa(index)) {
			t.Fatalf("output %d ID = %q", index, got)
		}
	}
}

func TestInboxDiscardsRecoveredIDs(t *testing.T) {
	inputs, err := inbox.New(t.Context(), []inbox.ID{"recovered", "recovered"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inputs.Submit(t.Context(), inbox.Input{
		ID: "recovered", Kind: inbox.InputExternal,
	}); err != nil {
		t.Fatal(err)
	}
	if got := submitAndReceive(t, inputs, inbox.Input{
		ID: "new", Kind: inbox.InputExternal,
	}); got.ID != "new" {
		t.Fatalf("output ID = %q, want new", got.ID)
	}
	assertNoInput(t, inputs.Output())
}

func TestInboxHonorsCanceledSubmissionContext(t *testing.T) {
	inputs := newInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := inputs.Submit(ctx, testInput("input-1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("submit error = %v, want context canceled", err)
	}
	if got := submitAndReceive(t, inputs, testInput("input-1")); got.ID != "input-1" {
		t.Fatalf("retried output ID = %q, want input-1", got.ID)
	}
}

func TestInboxClonesSubmittedInput(t *testing.T) {
	inputs := newInbox(t)
	input := inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal,
		Payload: jsontext.Value(`{"value":"original"}`),
	}
	got := submitAndReceive(t, inputs, input)
	input.Payload[10] = 'X'

	if payload := string(got.Payload); payload != `{"value":"original"}` {
		t.Fatalf("output payload = %s", payload)
	}
}

func TestInboxStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	inputs, err := inbox.New(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case _, open := <-inputs.Output():
		if open {
			t.Fatal("output remains open")
		}
	case <-time.After(time.Second):
		t.Fatal("output did not close")
	}
	if err := inputs.Submit(t.Context(), testInput("after-stop")); !errors.Is(err, context.Canceled) {
		t.Fatalf("submit error = %v, want context canceled", err)
	}
}

func TestInboxStopsWithQueuedInputs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	inputs, err := inbox.New(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 10 {
		if err := inputs.Submit(t.Context(), testInput(inbox.ID(strconv.Itoa(index)))); err != nil {
			t.Fatal(err)
		}
	}
	cancel()

	deadline := time.After(time.Second)
	for {
		select {
		case _, open := <-inputs.Output():
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("output did not close")
		}
	}
}

func TestInboxRejectsInvalidInput(t *testing.T) {
	inputs := newInbox(t)
	tests := []inbox.Input{
		{Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`)},
		{ID: "unknown-kind", Kind: "unknown"},
		{ID: "invalid-json", Kind: inbox.InputExternal, Payload: jsontext.Value(`{`)},
	}
	for _, input := range tests {
		if err := inputs.Submit(t.Context(), input); err == nil {
			t.Fatalf("Submit(%#v) succeeded", input)
		}
	}
}

func TestInboxRejectsInvalidSeenID(t *testing.T) {
	if _, err := inbox.New(t.Context(), []inbox.ID{""}); err == nil {
		t.Fatal("New succeeded with an empty seen ID")
	}
}

func newInbox(t *testing.T) *inbox.Inbox {
	t.Helper()
	inputs, err := inbox.New(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

func testInput(id inbox.ID) inbox.Input {
	return inbox.Input{ID: id, Kind: inbox.InputExternal}
}

func submitAndReceive(t *testing.T, inputs *inbox.Inbox, input inbox.Input) inbox.Input {
	t.Helper()
	if err := inputs.Submit(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	return receiveInput(t, inputs.Output())
}

func receiveInput(t *testing.T, output <-chan inbox.Input) inbox.Input {
	t.Helper()
	select {
	case input, open := <-output:
		if !open {
			t.Fatal("output closed")
		}
		return input
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for input")
		return inbox.Input{}
	}
}

func assertNoInput(t *testing.T, output <-chan inbox.Input) {
	t.Helper()
	select {
	case input := <-output:
		t.Fatalf("unexpected input: %#v", input)
	default:
	}
}
