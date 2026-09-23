package operation

import (
	"bytes"
	"context"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestViewImageDispatchesComputeAndRecoveryRereads(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(160, 120), "bmp")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 16, MaxHeight: 12})
	view := viewImageTestActor(t, current)
	start, err := view.Handle(nil)
	if err != nil {
		t.Fatal(err)
	}
	request := start.Dispatches[0].Data.(primitives.IOReadRequest)
	original := start.Operation.State.Clone()
	for offset := 0; offset < len(data); {
		end := min(offset+1024, len(data))
		step, err := view.Handle(&primitives.PrimitiveEvent{
			Type: primitives.PrimitiveEventIOReadOutput, Source: request.Source, CorrelationID: request.CorrelationID,
			Result: primitives.IOReadOutputResult{Offset: int64(offset), Data: data[offset:end]},
		})
		if err != nil || step.Operation != nil || len(step.Dispatches) != 0 {
			t.Fatalf("chunk processing did more than buffer data: %+v, %v", step, err)
		}
		offset = end
	}
	processing, err := view.Handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventIOReadCompleted, Source: request.Source, CorrelationID: request.CorrelationID,
		Result: primitives.IOReadCompletedResult{Size: int64(len(data))},
	})
	if err != nil {
		t.Fatal(err)
	}
	compute := viewImageComputeRequest(t, processing)
	if compute.Source != request.Source || compute.CorrelationID != viewImageProcessCorrelation ||
		view.chunks != nil || view.readSize != 0 || !view.processing || view.state.Result != nil {
		t.Fatal("processing did not transfer the source chunks to compute")
	}

	// The callback owns only transient inputs, even if the actor changes before it runs.
	config := view.state.Config
	view.state.Config.MaxSize = 1
	event := runViewImageCompute(t, processing)
	view.state.Config = config
	if view.state.Result != nil || view.current.Status != StatusAwaiting || !bytes.Equal(original, start.Operation.State) {
		t.Fatal("compute mutated the actor or its durable checkpoint")
	}
	completed, err := view.Handle(&event)
	if err != nil || completed.Operation == nil || completed.Operation.Status != StatusCompleted || len(completed.Dispatches) != 0 {
		t.Fatalf("processing event failed: %+v, %v", completed, err)
	}
	_, decoded := decodeViewImageContent(t, *view.state.Result, "png")
	if decoded.Bounds().Dx() != 16 || decoded.Bounds().Dy() != 12 || len(view.state.Result.Content) <= 1 {
		t.Fatal("callback did not use its captured configuration and source")
	}
	if _, err := view.Handle(&event); err == nil {
		t.Fatal("duplicate terminal event was accepted")
	}

	recovered := viewImageTestActor(t, *start.Operation)
	restarted, err := recovered.Handle(nil)
	if err != nil || restarted.Dispatches[0].Data.(primitives.IOReadRequest) != request {
		t.Fatalf("recovery did not reread the complete file: %+v, %v", restarted, err)
	}
	resumed, result := runViewImageTest(t, *start.Operation)
	if resumed.Status != StatusCompleted || result.OriginalWidth != 160 || result.ScaleRatio != 0.1 {
		t.Fatalf("recovery result = %+v", result)
	}
}

func TestViewImageCancellationDoesNotDispatchCompute(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(30, 20), "png")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	for _, phase := range []string{"reading", "computing", "converted"} {
		t.Run(phase, func(t *testing.T) {
			view := viewImageTestActor(t, current)
			correlation := viewImageReadCorrelation
			if phase == "reading" {
				if _, err := view.Handle(nil); err != nil {
					t.Fatal(err)
				}
				if _, err := view.Handle(&primitives.PrimitiveEvent{
					Type: primitives.PrimitiveEventIOReadOutput, Source: primitives.SourceID(current.ID), CorrelationID: correlation,
					Result: primitives.IOReadOutputResult{Data: data},
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				var step Step
				view, step = viewImageReadyToCompute(t, current, data)
				correlation = viewImageProcessCorrelation
				if phase == "converted" {
					runViewImageCompute(t, step)
				}
			}
			step, err := view.Handle(&primitives.PrimitiveEvent{
				Type: primitives.PrimitiveEventCanceled, Source: primitives.SourceID(current.ID), CorrelationID: correlation,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertViewImageCanceled(t, step)
			if view.chunks != nil || view.readSize != 0 || view.processing {
				t.Fatal("cancellation retained buffered input or started processing")
			}
		})
	}
	current.Status = StatusCanceling
	recovered := viewImageTestActor(t, current)
	step, err := recovered.Handle(nil)
	if err != nil {
		t.Fatal(err)
	}
	assertViewImageCanceled(t, step)
}

func TestViewImageValidatesEventsBeforeUpdatingState(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(30, 20), "png")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	for _, phase := range []string{"reading", "computing"} {
		t.Run(phase, func(t *testing.T) {
			view := viewImageTestActor(t, current)
			start, err := view.Handle(nil)
			if err != nil {
				t.Fatal(err)
			}
			correlation := viewImageReadCorrelation
			if phase == "computing" {
				view, _ = viewImageReadyToCompute(t, current, data)
				correlation = viewImageProcessCorrelation
				if step, err := view.Handle(nil); err == nil || step.Operation != nil || len(step.Dispatches) != 0 {
					t.Fatal("processing was restarted before its terminal event")
				}
			}
			original := start.Operation.State.Clone()
			for _, kind := range []primitives.PrimitiveEventType{
				primitives.PrimitiveEventComputeCompleted, primitives.PrimitiveEventFailed, primitives.PrimitiveEventCanceled,
			} {
				for _, scenario := range []string{"wrong source", "wrong correlation", "invalid payload"} {
					event := primitives.PrimitiveEvent{Type: kind, Source: primitives.SourceID(current.ID), CorrelationID: correlation}
					switch scenario {
					case "wrong source":
						event.Source = "another-operation"
					case "wrong correlation":
						event.CorrelationID = "another-invocation"
					case "invalid payload":
						event.Result = primitives.IOReadCompletedResult{}
					}
					if step, err := view.Handle(&event); err == nil || step.Operation != nil || len(step.Dispatches) != 0 || view.state.Result != nil {
						t.Fatalf("%s %s changed the actor: %+v, %v", kind, scenario, step, err)
					}
				}
			}
			if phase == "computing" {
				for _, event := range []primitives.PrimitiveEvent{
					{Type: primitives.PrimitiveEventComputeCompleted, Result: primitives.ComputeResult{Value: "not an image result"}},
					{Type: primitives.PrimitiveEventIOReadCompleted, Result: primitives.IOReadCompletedResult{Size: int64(len(data))}},
				} {
					event.Source, event.CorrelationID = primitives.SourceID(current.ID), correlation
					if step, err := view.Handle(&event); err == nil || step.Operation != nil || len(step.Dispatches) != 0 {
						t.Fatalf("invalid compute event changed the actor: %+v, %v", step, err)
					}
				}
			}
			if !bytes.Equal(original, start.Operation.State) {
				t.Fatal("invalid event mutated the previous checkpoint")
			}
		})
	}
}

func TestViewImageReadFailureWithoutDataDoesNotDispatchCompute(t *testing.T) {
	current := viewImageTestOperation(t, nil, ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	view := viewImageTestActor(t, current)
	if _, err := view.Handle(nil); err != nil {
		t.Fatal(err)
	}
	step, err := view.Handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventFailed, Source: primitives.SourceID(current.ID), CorrelationID: viewImageReadCorrelation,
		Result: primitives.PrimitiveFailureResult{Error: "file not found"},
	})
	if err != nil || step.Operation == nil || step.Operation.Status != StatusFailed || len(step.Dispatches) != 0 {
		t.Fatalf("read failure did not terminate immediately: %+v, %v", step, err)
	}
	state, err := DecodeViewImageState(*step.Operation)
	if err != nil || state.Result == nil || state.Result.Error != "read image: file not found" {
		t.Fatalf("read failure result = %+v, %v", state.Result, err)
	}
}

func TestViewImageReadFailureRetainsPartialHeaderDimensions(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(30, 20), "png")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	view := viewImageTestActor(t, current)
	if _, err := view.Handle(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventIOReadOutput, Source: primitives.SourceID(current.ID), CorrelationID: viewImageReadCorrelation,
		Result: primitives.IOReadOutputResult{Data: data[:33]},
	}); err != nil {
		t.Fatal(err)
	}
	processing, err := view.Handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventFailed, Source: primitives.SourceID(current.ID), CorrelationID: viewImageReadCorrelation,
		Result: primitives.PrimitiveFailureResult{Error: "read interrupted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	event := runViewImageCompute(t, processing)
	step, err := view.Handle(&event)
	if err != nil || step.Operation == nil || step.Operation.Status != StatusFailed || len(step.Dispatches) != 0 {
		t.Fatalf("read failure: %+v, %v", step, err)
	}
	result := view.state.Result
	if result.OriginalWidth != 30 || result.OriginalHeight != 20 || result.OriginalMIMEType != "image/png" ||
		result.Error != "read image: read interrupted" {
		t.Fatalf("partial read lost metadata: %+v", result)
	}
}

func TestViewImageComputeReportsProcessingPanic(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(30, 20), "png")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	view, processing := viewImageReadyToCompute(t, current, data)
	request := viewImageComputeRequest(t, processing)
	run := request.Run
	request.Run = func(ctx context.Context) (any, error) {
		return run(viewImagePanicContext{Context: ctx})
	}
	events := make(chan primitives.PrimitiveEvent, 1)
	primitives.Compute(t.Context(), request, events)
	event := <-events
	if event.Type != primitives.PrimitiveEventFailed {
		t.Fatalf("processing panic did not reach compute: %+v", event)
	}
	step, err := view.Handle(&event)
	if err != nil || step.Operation == nil || step.Operation.Status != StatusFailed || len(step.Dispatches) != 0 {
		t.Fatalf("panic did not produce a failure checkpoint: %+v, %v", step, err)
	}
	result := view.state.Result
	if result.Content != "" {
		t.Fatalf("panic retained encoded content: %+v", result)
	}
	if result.Error != "image primitive failed: compute callback panicked: injected reader panic" {
		t.Fatalf("panic error = %q", result.Error)
	}
}

type viewImagePanicContext struct {
	context.Context
}

func (viewImagePanicContext) Err() error {
	panic("injected reader panic")
}

func TestViewImageComputeFailuresTerminateWithoutRedispatch(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(30, 20), "png")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	for _, correlation := range []primitives.CorrelationID{viewImageProcessCorrelation, ""} {
		t.Run(string(correlation), func(t *testing.T) {
			view, _ := viewImageReadyToCompute(t, current, data)
			step, err := view.Handle(&primitives.PrimitiveEvent{
				Type: primitives.PrimitiveEventFailed, Source: primitives.SourceID(current.ID), CorrelationID: correlation,
				Result: primitives.PrimitiveFailureResult{Error: "compute failed"},
			})
			if err != nil || step.Operation == nil || step.Operation.Status != StatusFailed || len(step.Dispatches) != 0 {
				t.Fatalf("failure did not terminate: %+v, %v", step, err)
			}
			state, err := DecodeViewImageState(*step.Operation)
			if err != nil || state.Result == nil || state.Result.Content != "" ||
				state.Result.Error != "image primitive failed: compute failed" {
				t.Fatalf("failure result = %+v, %v", state.Result, err)
			}
		})
	}
}

func viewImageTestActor(t *testing.T, current Operation) *ViewImage {
	t.Helper()
	view, err := NewViewImage(current)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func viewImageReadyToCompute(t *testing.T, current Operation, data []byte) (*ViewImage, Step) {
	t.Helper()
	view := viewImageTestActor(t, current)
	if _, err := view.Handle(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventIOReadOutput, Source: primitives.SourceID(current.ID), CorrelationID: viewImageReadCorrelation,
		Result: primitives.IOReadOutputResult{Data: data},
	}); err != nil {
		t.Fatal(err)
	}
	step, err := view.Handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventIOReadCompleted, Source: primitives.SourceID(current.ID), CorrelationID: viewImageReadCorrelation,
		Result: primitives.IOReadCompletedResult{Size: int64(len(data))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return view, step
}

func viewImageComputeRequest(t *testing.T, step Step) primitives.ComputeRequest {
	t.Helper()
	if step.Operation != nil || len(step.Dispatches) != 1 || step.Dispatches[0].Type != primitives.PrimitiveDispatchCompute {
		t.Fatalf("processing did not return only a compute dispatch: %+v", step)
	}
	request, ok := step.Dispatches[0].Data.(primitives.ComputeRequest)
	if !ok || request.Run == nil {
		t.Fatalf("invalid compute request: %+v", step.Dispatches[0].Data)
	}
	return request
}

func runViewImageCompute(t *testing.T, step Step) primitives.PrimitiveEvent {
	t.Helper()
	request := viewImageComputeRequest(t, step)
	value, err := request.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventComputeCompleted, Source: request.Source, CorrelationID: request.CorrelationID,
		Result: primitives.ComputeResult{Value: value},
	}
}

func assertViewImageCanceled(t *testing.T, step Step) {
	t.Helper()
	if step.Operation == nil || step.Operation.Status != StatusCanceled || len(step.Dispatches) != 0 {
		t.Fatalf("cancellation did not terminate: %+v", step)
	}
	state, err := DecodeViewImageState(*step.Operation)
	if err != nil || state.Result == nil || state.Result.Content != "" ||
		state.Result.Error != "view-image operation canceled: context canceled" {
		t.Fatalf("cancellation result = %+v, %v", state.Result, err)
	}
}
