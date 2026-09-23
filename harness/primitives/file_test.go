package primitives_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
	"golang.org/x/sys/unix"
)

func TestReadFileStreamsRangeAndCompletesWithFullSize(t *testing.T) {
	contents := make([]byte, primitives.IOReadChunkSize*3+17)
	for index := range contents {
		contents[index] = byte(index % 251)
	}
	path := writeTestFile(t, contents)
	offset := int64(primitives.IOReadChunkSize / 2)
	count := int64(primitives.IOReadChunkSize*2 + 11)
	request := primitives.IOReadRequest{
		Source:        "operation-1",
		CorrelationID: "read-1",
		Path:          path,
		Offset:        offset,
		Count:         count,
	}

	var output []byte
	nextOffset := offset
	outputEvents := 0
	var completed primitives.IOReadCompletedResult
	for _, event := range collectEvents(readFile(t.Context(), request)) {
		if event.Source != request.Source || event.CorrelationID != request.CorrelationID {
			t.Fatalf("event identity = (%q, %q)", event.Source, event.CorrelationID)
		}

		switch event.Type {
		case primitives.PrimitiveEventIOReadOutput:
			result := eventResult[primitives.IOReadOutputResult](t, event)
			if result.Offset != nextOffset {
				t.Fatalf("output offset = %d, want %d", result.Offset, nextOffset)
			}
			if len(result.Data) > primitives.IOReadChunkSize {
				t.Fatalf("output size = %d, exceeds chunk size %d", len(result.Data), primitives.IOReadChunkSize)
			}
			output = append(output, result.Data...)
			nextOffset += int64(len(result.Data))
			outputEvents++
		case primitives.PrimitiveEventIOReadCompleted:
			completed = eventResult[primitives.IOReadCompletedResult](t, event)
		default:
			t.Fatalf("unexpected event type %q", event.Type)
		}
	}

	if outputEvents < 2 {
		t.Fatalf("output events = %d, want a streamed result", outputEvents)
	}
	want := contents[offset : offset+count]
	if !bytes.Equal(output, want) {
		t.Fatalf("streamed output does not match requested range")
	}
	if completed.Size != int64(len(contents)) {
		t.Fatalf("completed size = %d, want %d", completed.Size, len(contents))
	}
}

func TestReadFileClampsRangeToEndOfFile(t *testing.T) {
	contents := []byte("0123456789")
	path := writeTestFile(t, contents)
	request := primitives.IOReadRequest{
		Path:   path,
		Offset: 7,
		Count:  math.MaxInt64,
	}

	var output []byte
	for _, event := range collectEvents(readFile(t.Context(), request)) {
		if event.Type == primitives.PrimitiveEventIOReadOutput {
			output = append(output, eventResult[primitives.IOReadOutputResult](t, event).Data...)
		}
	}

	if string(output) != "789" {
		t.Fatalf("output = %q, want %q", output, "789")
	}
}

func TestReadFileWithZeroCountEmitsOnlyCompletion(t *testing.T) {
	contents := []byte("contents")
	path := writeTestFile(t, contents)
	request := primitives.IOReadRequest{Path: path}

	events := collectEvents(readFile(t.Context(), request))
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Type != primitives.PrimitiveEventIOReadCompleted {
		t.Fatalf("event type = %q, want %q", events[0].Type, primitives.PrimitiveEventIOReadCompleted)
	}
	completed := eventResult[primitives.IOReadCompletedResult](t, events[0])
	if completed.Size != int64(len(contents)) {
		t.Fatalf("completed size = %d, want %d", completed.Size, len(contents))
	}
}

func TestReadFileCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	events := collectEvents(readFile(ctx, primitives.IOReadRequest{Path: "unused"}))
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventCanceled {
		t.Fatalf("events = %#v, want one cancellation", events)
	}
}

func TestReadFileCancellationTerminatesStream(t *testing.T) {
	contents := bytes.Repeat([]byte("a"), primitives.IOReadChunkSize*3)
	path := writeTestFile(t, contents)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := primitives.IOReadRequest{
		Source:        "operation-1",
		CorrelationID: "read-1",
		Path:          path,
		Count:         int64(len(contents)),
	}

	outputEvents := 0
	canceledEvents := 0
	events := readFile(ctx, request)

stream:
	for {
		event := <-events
		switch event.Type {
		case primitives.PrimitiveEventIOReadOutput:
			outputEvents++
			cancel()
		case primitives.PrimitiveEventCanceled:
			canceledEvents++
			break stream
		case primitives.PrimitiveEventIOReadCompleted:
			t.Fatal("canceled read completed")
		default:
			t.Fatalf("unexpected event type %q", event.Type)
		}
	}

	if outputEvents == 0 || outputEvents >= 3 {
		t.Fatalf("output events = %d, want cancellation before the complete stream", outputEvents)
	}
	if canceledEvents != 1 {
		t.Fatalf("canceled events = %d, want 1", canceledEvents)
	}
}

func TestReadFileFailureIsTerminal(t *testing.T) {
	request := primitives.IOReadRequest{
		Source:        "operation-1",
		CorrelationID: "read-1",
		Path:          filepath.Join(t.TempDir(), "missing"),
		Count:         1,
	}

	events := collectEvents(readFile(t.Context(), request))
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Type != primitives.PrimitiveEventFailed {
		t.Fatalf("event type = %q, want %q", events[0].Type, primitives.PrimitiveEventFailed)
	}
	result := eventResult[primitives.PrimitiveFailureResult](t, events[0])
	if !strings.Contains(result.Error, "missing") {
		t.Fatalf("error = %q, want missing path", result.Error)
	}
}

func TestReadFileIgnoresAdvisoryFileLock(t *testing.T) {
	contents := []byte("locked contents")
	path := writeTestFile(t, contents)
	locked, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(locked.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Flock(int(locked.Fd()), unix.LOCK_UN); err != nil {
			t.Error(err)
		}
		if err := locked.Close(); err != nil {
			t.Error(err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	events := collectEvents(readFile(ctx, primitives.IOReadRequest{
		Path:  path,
		Count: int64(len(contents)),
	}))
	if len(events) != 2 {
		t.Fatalf("events = %#v, want output and completion", events)
	}
	if events[0].Type != primitives.PrimitiveEventIOReadOutput ||
		events[1].Type != primitives.PrimitiveEventIOReadCompleted {
		t.Fatalf("event types = (%q, %q), want output and completion", events[0].Type, events[1].Type)
	}
}

func TestReadFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	events := readFile(ctx, primitives.IOReadRequest{Path: path, Count: 1})
	select {
	case event := <-events:
		if event.Type != primitives.PrimitiveEventFailed {
			t.Fatalf("event type = %q, want %q", event.Type, primitives.PrimitiveEventFailed)
		}
		result := eventResult[primitives.PrimitiveFailureResult](t, event)
		if !strings.Contains(result.Error, "unsupported file type") {
			t.Fatalf("error = %q", result.Error)
		}
	case <-ctx.Done():
		t.Fatal("opening FIFO blocked")
	}
	select {
	case _, open := <-events:
		if !open {
			t.Fatal("primitive closed the caller-owned channel")
		}
		t.Fatal("unexpected event after failure")
	default:
	}
}

func TestReadFileRejectsNegativeRange(t *testing.T) {
	request := primitives.IOReadRequest{
		Path:   "unused",
		Offset: -1,
	}

	events := collectEvents(readFile(t.Context(), request))
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventFailed {
		t.Fatalf("events = %#v, want one failure", events)
	}
	result := eventResult[primitives.PrimitiveFailureResult](t, events[0])
	if !strings.Contains(result.Error, "offset must not be negative") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestReadFileRejectsNegativeCount(t *testing.T) {
	request := primitives.IOReadRequest{
		Path:  "unused",
		Count: -1,
	}

	events := collectEvents(readFile(t.Context(), request))
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventFailed {
		t.Fatalf("events = %#v, want one failure", events)
	}
	result := eventResult[primitives.PrimitiveFailureResult](t, events[0])
	if !strings.Contains(result.Error, "count must not be negative") {
		t.Fatalf("error = %q", result.Error)
	}
}

func writeTestFile(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
