package agentrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/coordinator"
	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore/localfile"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

func FuzzCoordinatorLogMatchesExecution(f *testing.F) {
	addLogFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, actions []byte, text string, input, cached, written, output, reasoning uint64) {
		if len(actions) > 16 || len(text) > 128<<10 || len(actions)*len(text) > 256<<10 {
			t.Skip()
		}
		synctest.Test(t, func(t *testing.T) {
			text = strings.ToValidUTF8(text, "\uFFFD")
			usage := fuzzLogUsage(t, text, input, cached, written, output, reasoning)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			const id session.ID = "coordinator-execution"
			directory := t.TempDir()
			store, err := localfile.New(directory)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(ctx, id); err != nil {
				t.Fatal(err)
			}
			inputs, err := inbox.New(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			file, err := openDatetimeLog(t.TempDir(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var stdout bytes.Buffer
			observer := &sessionObserver{sessionID: id, output: io.MultiWriter(file, &stdout), cancel: cancel}
			observerID := store.AddObserver(observer.Observe)
			defer store.RemoveObserver(observerID)
			builder := contextbuilder.NewBuilder()
			builder.SetModel(llm.Model{ID: "journal-model"})
			model := &gatedLogModel{calls: make(chan *logModelCall), returned: make(map[int]llm.Response)}
			current := coordinator.New(coordinator.Dependencies{
				SessionID: id, Inbox: inputs, Sessions: store, ContextBuilder: builder,
				LLM: model, Tools: tool.NewRegistry(tool.StaticTranslators{}),
				Operations: operation.NewLocalOperationManager(ctx),
			})

			var submitted []inbox.Input
			var messages []string
			submit := func(value inbox.Input) {
				t.Helper()
				source := value
				source.Payload = source.Payload.Clone()
				if err := inputs.Submit(ctx, value); err != nil {
					t.Fatal(err)
				}
				submitted = append(submitted, source)
			}
			submitMessage := func() {
				t.Helper()
				message := fmt.Sprintf("%d: %s", len(messages), text)
				submit(inbox.Input{ID: inbox.ID(fmt.Sprintf("input-%d", len(messages))), Kind: inbox.InputExternal, Payload: logJSON(t, message)})
				messages = append(messages, message)
			}
			submitMessage()
			done := make(chan error, 1)
			go func() { done <- current.Run(ctx) }()
			requests := 0
			nextCall := func() *logModelCall {
				t.Helper()
				select {
				case call := <-model.calls:
					if call.index != requests {
						t.Fatalf("model request index = %d, want %d", call.index, requests)
					}
					requests++
					var delivered []string
					for _, item := range call.request.Input {
						if message, ok := item.Data.(llm.Message); ok && message.Role == llm.RoleUser {
							delivered = append(delivered, message.Text)
						}
					}
					if !reflect.DeepEqual(delivered, messages) {
						t.Fatal("model request lost, changed, or duplicated submitted messages")
					}
					return call
				case err := <-done:
					t.Fatalf("coordinator stopped before the next model request: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				return nil
			}
			complete := func(call *logModelCall, variant byte) {
				t.Helper()
				response := fuzzLogResponse(t, text, usage, call.index, variant)
				select {
				case call.response <- response:
				case <-call.ctx.Done():
					t.Fatal("model request was interrupted before completion")
				}
				// The coordinator cancels the request context when it accepts its
				// result. Wait for that boundary before submitting the next input.
				<-call.ctx.Done()
				synctest.Wait()
			}
			pending := nextCall()
			for _, action := range actions {
				switch action % 3 {
				case 0:
					complete(pending, action)
					submitMessage()
					pending = nextCall()
				case 1:
					interrupted := pending
					submitMessage()
					pending = nextCall()
					if !errors.Is(interrupted.ctx.Err(), context.Canceled) {
						t.Fatal("steering did not interrupt the pending model request")
					}
				case 2:
					duplicate := submitted[len(submitted)-1]
					duplicate.Payload = logJSON(t, "duplicate: "+text)
					if err := inputs.Submit(ctx, duplicate); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					if pending.ctx.Err() != nil {
						t.Fatal("duplicate input interrupted the model request")
					}
				}
			}
			mode := byte(0)
			if len(actions) > 0 {
				mode = actions[len(actions)-1] % 3
			}
			var wantErr error
			if mode == 2 {
				cancel()
				wantErr = context.Canceled
			} else {
				stop := inbox.StopWhenIdle
				if mode == 1 {
					stop = inbox.StopHard
				}
				submit(inbox.Input{ID: "stop", Kind: inbox.InputControl, Payload: logJSON(t, inbox.ControlMessage{Mode: stop, Reason: text})})
				if mode == 0 {
					complete(pending, byte(len(actions)))
				}
			}
			if err := <-done; !errors.Is(err, wantErr) {
				t.Fatalf("coordinator error = %v, want %v", err, wantErr)
			}
			cancel()
			synctest.Wait()
			if err := observer.Err(); err != nil {
				t.Fatal(err)
			}
			logged, err := os.ReadFile(file.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(logged, stdout.Bytes()) {
				t.Fatal("file log differs from stdout")
			}
			items := decodeLogItems(t, logged)
			reopened, err := localfile.New(directory)
			if err != nil {
				t.Fatal(err)
			}
			if history := readLogHistory(t, reopened, id, 1+len(actions)%7); !reflect.DeepEqual(history, items) {
				t.Fatal("file log differs from reopened session history")
			}
			if model.requests != requests {
				t.Fatalf("model made %d requests, driver received %d", model.requests, requests)
			}
			assertCoordinatorExecutionLog(t, items, submitted, requests, model.returned)
		})
	})
}

type logModelCall struct {
	index    int
	ctx      context.Context
	request  llm.Request
	response chan llm.Response
}

type gatedLogModel struct {
	calls chan *logModelCall

	mu       sync.Mutex
	requests int
	returned map[int]llm.Response
}

func (model *gatedLogModel) Respond(ctx context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	model.mu.Lock()
	index := model.requests
	model.requests++
	model.mu.Unlock()
	call := &logModelCall{index: index, ctx: ctx, request: request, response: make(chan llm.Response)}
	select {
	case model.calls <- call:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	select {
	case response := <-call.response:
		model.mu.Lock()
		model.returned[index] = copyLogResponse(response)
		model.mu.Unlock()
		return response, nil
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}

func assertCoordinatorExecutionLog(t *testing.T, items []sessionstore.Item, submitted []inbox.Input, requests int, returned map[int]llm.Response) {
	t.Helper()
	var inputs []inbox.Input
	var previous session.TurnID
	var recordedAt time.Time
	turns := 0
	responses := make(map[int]llm.Response)
	turnIDs := make(map[session.TurnID]bool)
	for index, item := range items {
		if item.Sequence != sessionstore.Sequence(index+1) || item.RecordedAt.IsZero() || item.RecordedAt.Before(recordedAt) {
			t.Fatalf("invalid sequence or timestamp at item %d: %#v", index, item)
		}
		recordedAt = item.RecordedAt
		switch value := item.Data.(type) {
		case inbox.Input:
			inputs = append(inputs, value)
		case session.Turn:
			if value.ID == "" || turnIDs[value.ID] || value.PreviousTurnID != previous || value.Type != session.TurnRegular {
				t.Fatalf("invalid turn chain: %#v after %q", value, previous)
			}
			previous = value.ID
			turnIDs[value.ID] = true
			turns++
		case sessionstore.ModelResponse:
			if _, exists := responses[turns-1]; exists || turns == 0 || value.TurnID != previous {
				t.Fatalf("response duplicated or attached to the wrong turn: %#v", value)
			}
			responses[turns-1] = value.Response
		default:
			t.Fatalf("unexpected logged item: %#v", item)
		}
	}
	if !reflect.DeepEqual(inputs, submitted) {
		t.Fatal("logged inputs lost, changed, or duplicated submitted inputs")
	}
	if turns != requests {
		t.Fatalf("logged turns = %d, actual model requests = %d", turns, requests)
	}
	if !reflect.DeepEqual(responses, returned) {
		t.Fatalf("logged responses differ from completed model responses by request (including usage):\ngot  %s\nwant %s", logJSON(t, responses), logJSON(t, returned))
	}
}
