package operation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestViewImageRecognizesFormatsFromContent(t *testing.T) {
	source := viewImageFixture(24, 12)
	for _, format := range []string{"jpeg", "png", "bmp", "tiff", "webp"} {
		t.Run(format, func(t *testing.T) {
			data := encodeViewImageFixture(t, source, format)
			width, height := 24, 12
			if format == "webp" {
				width, height = 3, 2
			}
			current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 100_000, MaxWidth: 100, MaxHeight: 100})
			completed, result := runViewImageTest(t, current)
			if completed.Status != StatusCompleted || result.Error != "" {
				t.Fatalf("status = %q, error = %q", completed.Status, result.Error)
			}
			if result.OriginalWidth != width || result.OriginalHeight != height || result.OriginalMIMEType != "image/"+format || result.ScaleRatio != 1 {
				t.Fatalf("metadata = %+v", result)
			}
			wantFormat := "png"
			if format == "jpeg" {
				wantFormat = "jpeg"
			}
			encoded, actual := decodeViewImageContent(t, result, wantFormat)
			if actual.Bounds().Dx() != width || actual.Bounds().Dy() != height {
				t.Fatalf("encoded dimensions = %v", actual.Bounds())
			}
			if format == "jpeg" && !bytes.Equal(encoded, data) {
				t.Fatal("unresized JPEG was re-encoded")
			}
			if format == "png" || format == "bmp" || format == "tiff" {
				for y := range height {
					for x := range width {
						if color.NRGBAModel.Convert(actual.At(x, y)) != color.NRGBAModel.Convert(source.At(x, y)) {
							t.Fatalf("pixel changed at %d, %d", x, y)
						}
					}
				}
			}
		})
	}
}

func TestViewImageRejectsGIF(t *testing.T) {
	frame := image.NewPaletted(image.Rect(6, 4, 14, 8), color.Palette{color.Black, color.White})
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{
		Image: []*image.Paletted{frame, frame}, Delay: []int{10, 10},
		Config: image.Config{ColorModel: frame.Palette, Width: 24, Height: 12},
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"static", encodeViewImageFixture(t, viewImageFixture(24, 12), "gif")},
		{"animated", animated.Bytes()},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := viewImageTestOperation(t, test.data, ViewImageConfig{MaxSize: 1000, MaxWidth: 12, MaxHeight: 12})
			completed, result := runViewImageTest(t, current)
			if completed.Status != StatusFailed || result.Content != "" || result.EncodedMIMEType != "" ||
				result.OriginalWidth != 24 || result.OriginalHeight != 12 || result.OriginalMIMEType != "image/gif" || result.ScaleRatio != 1 {
				t.Fatalf("status = %s, result = %+v", completed.Status, result)
			}
			if result.Error != `unsupported image format "gif"` {
				t.Fatalf("unexpected GIF error: %s", result.Error)
			}
		})
	}
}

func TestViewImageResizesToBothBounds(t *testing.T) {
	for _, test := range []struct {
		name                               string
		width, height, maxWidth, maxHeight int
		wantWidth, wantHeight              int
		ratio                              float64
	}{
		{"landscape", 120, 60, 40, 40, 40, 20, 1.0 / 3},
		{"portrait", 60, 120, 40, 40, 20, 40, 1.0 / 3},
		{"width limits portrait", 60, 120, 10, 100, 10, 20, 1.0 / 6},
		{"height limits landscape", 120, 60, 100, 10, 20, 10, 1.0 / 6},
		{"square", 60, 60, 30, 40, 30, 30, 0.5},
		{"already fits", 30, 20, 30, 40, 30, 20, 1},
		{"no upscale", 3, 2, 100, 100, 3, 2, 1},
		{"round pixels", 7, 3, 4, 4, 4, 2, 4.0 / 7},
		{"thin image", 100, 1, 1, 1, 1, 1, 0.01},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, format := range []string{"png", "jpeg"} {
				data := encodeViewImageFixture(t, viewImageFixture(test.width, test.height), format)
				current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 100_000, MaxWidth: test.maxWidth, MaxHeight: test.maxHeight})
				completed, result := runViewImageTest(t, current)
				if completed.Status != StatusCompleted {
					t.Fatalf("%s: %s", format, result.Error)
				}
				_, actual := decodeViewImageContent(t, result, format)
				if actual.Bounds().Dx() != test.wantWidth || actual.Bounds().Dy() != test.wantHeight ||
					result.ScaleRatio != test.ratio || result.OriginalWidth != test.width || result.OriginalHeight != test.height {
					t.Fatalf("%s: dimensions = %v, metadata = %+v", format, actual.Bounds(), result)
				}
			}
		})
	}
}

func TestViewImagePreservesTransparency(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 8, 4))
	for y := range 4 {
		for x := range 8 {
			source.SetNRGBA(x, y, color.NRGBA{R: 120, G: 60, B: 30, A: 128})
		}
	}
	data := encodeViewImageFixture(t, source, "png")
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 4, MaxHeight: 4})
	_, result := runViewImageTest(t, current)
	_, decoded := decodeViewImageContent(t, result, "png")
	_, _, _, alpha := decoded.At(1, 1).RGBA()
	if alpha != 128*257 {
		t.Fatalf("alpha = %d, want %d", alpha, 128*257)
	}
}

func TestViewImageMaxSizeCountsBase64AfterResizing(t *testing.T) {
	for _, format := range []string{"jpeg", "bmp"} {
		t.Run(format, func(t *testing.T) {
			data := encodeViewImageFixture(t, viewImageFixture(120, 80), format)
			config := ViewImageConfig{MaxSize: 100_000, MaxWidth: 30, MaxHeight: 30}
			_, baseline := runViewImageTest(t, viewImageTestOperation(t, data, config))
			size := len(baseline.Content)
			if size == 0 {
				t.Fatalf("baseline failed: %s", baseline.Error)
			}
			for _, limit := range []int{size, size - 1, 1} {
				config.MaxSize = limit
				completed, result := runViewImageTest(t, viewImageTestOperation(t, data, config))
				if limit == size {
					if completed.Status != StatusCompleted || result.Content != baseline.Content {
						t.Fatalf("exact MaxSize rejected: %s", result.Error)
					}
					continue
				}
				if completed.Status != StatusFailed || result.Content != "" || result.OriginalWidth != 120 || result.OriginalHeight != 80 ||
					result.OriginalMIMEType != "image/"+format || result.EncodedMIMEType != baseline.EncodedMIMEType || result.ScaleRatio != 0.25 {
					t.Fatalf("oversized result = %+v, status = %s", result, completed.Status)
				}
				for _, text := range []string{"120x80 to 30x20", fmt.Sprintf("%d base64 bytes", size),
					fmt.Sprintf("MaxSize is %d bytes", limit), fmt.Sprintf("exceeded by %d bytes", size-limit)} {
					if !strings.Contains(result.Error, text) {
						t.Fatalf("error %q missing %q", result.Error, text)
					}
				}
			}
		})
	}
}

func TestViewImageRejectsOversizedSourceBeforeDecoding(t *testing.T) {
	header := make([]byte, 54)
	copy(header, "BM")
	binary.LittleEndian.PutUint32(header[2:], 54)
	binary.LittleEndian.PutUint32(header[10:], 54)
	binary.LittleEndian.PutUint32(header[14:], 40)
	binary.LittleEndian.PutUint32(header[18:], 1_000_000_000)
	binary.LittleEndian.PutUint32(header[22:], 1_000_000_000)
	binary.LittleEndian.PutUint16(header[26:], 1)
	binary.LittleEndian.PutUint16(header[28:], 24)
	current := viewImageTestOperation(t, header, ViewImageConfig{MaxSize: 1000, MaxWidth: 1, MaxHeight: 1})
	completed, result := runViewImageTest(t, current)
	if completed.Status != StatusFailed || result.Content != "" || result.EncodedMIMEType != "" ||
		result.OriginalWidth != 1_000_000_000 || result.OriginalHeight != 1_000_000_000 || result.OriginalMIMEType != "image/bmp" {
		t.Fatalf("oversized source: status = %s, result = %+v", completed.Status, result)
	}
	for _, message := range []string{"refuse to decode 1000000000x1000000000 bmp image", "MaxSourcePixels 32000000"} {
		if !strings.Contains(result.Error, message) {
			t.Fatalf("error %q missing %q", result.Error, message)
		}
	}
}

func TestViewImageSourcePixelBudgetBoundary(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(8, 4), "bmp")
	for _, maxPixels := range []int{31, 32, 33} {
		current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 1, MaxHeight: 1, MaxSourcePixels: maxPixels})
		completed, result := runViewImageTest(t, current)
		if maxPixels < 32 {
			if completed.Status != StatusFailed || !strings.Contains(result.Error, "MaxSourcePixels 31") || result.ScaleRatio != 1 {
				t.Fatalf("source budget was ignored: status = %s, result = %+v", completed.Status, result)
			}
			continue
		}
		if completed.Status != StatusCompleted || result.ScaleRatio != 0.125 {
			t.Fatalf("source within budget rejected: status = %s, result = %+v", completed.Status, result)
		}
		_, decoded := decodeViewImageContent(t, result, "png")
		if decoded.Bounds() != image.Rect(0, 0, 1, 1) {
			t.Fatalf("output bounds = %v", decoded.Bounds())
		}
	}
}

func TestViewImageSourceByteLimit(t *testing.T) {
	pngData := encodeViewImageFixture(t, viewImageFixture(1, 1), "png")
	for _, test := range []struct {
		name   string
		size   int64
		data   []byte
		status Status
	}{
		{"below limit", MaxViewImageSourceBytes - 1, pngData, StatusCompleted},
		{"at limit", MaxViewImageSourceBytes, pngData, StatusCompleted},
		{"oversized PNG", MaxViewImageSourceBytes + 1, pngData, StatusFailed},
		{"oversized invalid file", MaxViewImageSourceBytes + 123, []byte("not an image"), StatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := viewImageTestOperation(t, test.data, ViewImageConfig{MaxSize: 1024, MaxWidth: 1, MaxHeight: 1, MaxSourcePixels: 1})
			view := viewImageTestActor(t, current)
			start, err := view.Handle(nil)
			if err != nil {
				t.Fatal(err)
			}
			request := start.Dispatches[0].Data.(primitives.IOReadRequest)
			if request.Count != 256*1024*1024 || request.Offset != 0 {
				t.Fatalf("source read is not capped at 256 MiB: %+v", request)
			}
			// Reuse immutable padding to exercise the boundary without allocating 256 MiB.
			padding := make([]byte, primitives.IOReadChunkSize)
			chunk := test.data
			readSize := min(test.size, request.Count)
			for offset := int64(0); offset < readSize; {
				chunk = chunk[:min(int64(len(chunk)), readSize-offset)]
				step, err := view.Handle(&primitives.PrimitiveEvent{
					Type: primitives.PrimitiveEventIOReadOutput, Source: request.Source, CorrelationID: request.CorrelationID,
					Result: primitives.IOReadOutputResult{Offset: offset, Data: chunk},
				})
				if err != nil || step.Operation != nil || view.processing {
					t.Fatalf("source read failed before completion: %+v, %v", step, err)
				}
				offset += int64(len(chunk))
				chunk = padding
			}
			if view.readSize != readSize {
				t.Fatalf("buffered %d bytes, want %d", view.readSize, readSize)
			}
			processing, err := view.Handle(&primitives.PrimitiveEvent{
				Type: primitives.PrimitiveEventIOReadCompleted, Source: request.Source, CorrelationID: request.CorrelationID,
				Result: primitives.IOReadCompletedResult{Size: test.size},
			})
			if err != nil {
				t.Fatal(err)
			}
			event := runViewImageCompute(t, processing)
			finished, err := view.Handle(&event)
			if err != nil || finished.Operation == nil || finished.Operation.Status != test.status {
				t.Fatalf("source processing: %+v, %v", finished, err)
			}
			result := view.state.Result
			if bytes.Equal(test.data, pngData) && (result.OriginalWidth != 1 || result.OriginalHeight != 1 || result.OriginalMIMEType != "image/png") {
				t.Fatalf("lost original metadata: %+v", result)
			}
			if test.status == StatusCompleted {
				decodeViewImageContent(t, *result, "png")
				return
			}
			if result.Content != "" || result.EncodedMIMEType != "" {
				t.Fatalf("oversized source was encoded: %+v", result)
			}
			for _, message := range []string{
				fmt.Sprintf("image source is %d bytes", test.size), "source limit is 268435456 bytes",
				fmt.Sprintf("exceeded by %d bytes", test.size-MaxViewImageSourceBytes),
			} {
				if !strings.Contains(result.Error, message) {
					t.Fatalf("error %q missing %q", result.Error, message)
				}
			}
		})
	}
}

func TestViewImageFailuresRetainAvailableMetadata(t *testing.T) {
	data := encodeViewImageFixture(t, viewImageFixture(17, 11), "png")
	for _, test := range []struct {
		name, message string
		data          []byte
		width, height int
	}{
		{"empty", "decode image header", nil, 0, 0},
		{"unknown", "unknown format", []byte("not an image"), 0, 0},
		{"truncated pixels", "decode png pixels", data[:33], 17, 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := viewImageTestOperation(t, test.data, ViewImageConfig{MaxSize: 1000, MaxWidth: 100, MaxHeight: 100})
			completed, result := runViewImageTest(t, current)
			if completed.Status != StatusFailed || result.Content != "" || result.OriginalWidth != test.width || result.OriginalHeight != test.height {
				t.Fatalf("status = %s, result = %+v", completed.Status, result)
			}
			if !strings.Contains(result.Error, test.message) || strings.Contains(result.Error, "MaxSize") {
				t.Fatalf("unexpected image error: %s", result.Error)
			}
		})
	}
	current := viewImageTestOperation(t, data, ViewImageConfig{MaxSize: 1000, MaxWidth: 100, MaxHeight: 100})
	state, err := DecodeViewImageState(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state.Path); err != nil {
		t.Fatal(err)
	}
	_, readErr := os.ReadFile(state.Path)
	if !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("expected missing file: %v", readErr)
	}
	completed, result := runViewImageTest(t, current)
	wantError := fmt.Sprintf("read image: open %q: %v", state.Path, readErr)
	if completed.Status != StatusFailed || result.Error != wantError {
		t.Fatalf("missing file result = %+v", result)
	}
}

func TestViewImageSpecRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		path   string
		config ViewImageConfig
	}{
		{"", ViewImageConfig{MaxSize: 1, MaxWidth: 1, MaxHeight: 1}},
		{"image", ViewImageConfig{MaxSize: 0, MaxWidth: 1, MaxHeight: 1}},
		{"image", ViewImageConfig{MaxSize: 1, MaxWidth: 0, MaxHeight: 1}},
		{"image", ViewImageConfig{MaxSize: 1, MaxWidth: 1, MaxHeight: 0}},
		{"image", ViewImageConfig{MaxSize: -1, MaxWidth: 1, MaxHeight: 1}},
		{"image", ViewImageConfig{MaxSize: 1, MaxWidth: -1, MaxHeight: 1}},
		{"image", ViewImageConfig{MaxSize: 1, MaxWidth: 1, MaxHeight: -1}},
		{"image", ViewImageConfig{MaxSize: 1, MaxWidth: 1, MaxHeight: 1, MaxSourcePixels: -1}},
	} {
		if _, err := NewViewImageSpec(test.path, test.config); err == nil {
			t.Fatalf("accepted invalid config %+v", test)
		}
	}
}

func TestLocalOperationManagerViewImageHandleFailure(t *testing.T) {
	spec, err := NewViewImageSpec("image.jpg", ViewImageConfig{MaxSize: 1000, MaxWidth: 10, MaxHeight: 10})
	if err != nil {
		t.Fatal(err)
	}
	view := viewImageTestActor(t, Operation{
		ID: "view-image", Type: spec.Type, Version: spec.Version, State: spec.State, Status: StatusReady,
	})
	start, err := view.Handle(nil)
	if err != nil {
		t.Fatal(err)
	}
	original := start.Operation.State.Clone()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	current := &localRunningOperation{
		ctx: ctx, cancel: cancel, operation: *start.Operation, handle: view.Handle,
	}
	_, handleErr := current.handle(&primitives.PrimitiveEvent{
		Type: primitives.PrimitiveEventComputeCompleted, Source: primitives.SourceID(current.operation.ID),
		CorrelationID: viewImageProcessCorrelation,
	})
	if handleErr == nil {
		t.Fatal("unexpected processing event was accepted")
	}
	manager := &LocalOperationManager{}
	operations := map[ID]*localRunningOperation{current.operation.ID: current}
	manager.failLocalOperation(operations, current, handleErr)
	if len(manager.pendingUpdates) != 1 || len(operations) != 0 || ctx.Err() == nil {
		t.Fatal("failed operation was not published and canceled")
	}
	failed := manager.pendingUpdates[0]
	state, err := DecodeViewImageState(failed)
	want := ViewImageResult{
		ScaleRatio: 1, Error: handleErr.Error(),
	}
	if err != nil || failed.Status != StatusFailed || state.Result == nil || *state.Result != want {
		t.Fatalf("failure result: status = %s, result = %+v, error = %v", failed.Status, state.Result, err)
	}
	if !bytes.Equal(start.Operation.State, original) {
		t.Fatal("failure mutated the previous checkpoint")
	}
}

func TestImageContentWriterCountsBeyondLimit(t *testing.T) {
	writer := imageContentWriter{ctx: t.Context(), limit: 5}
	for _, data := range []string{"abc", "de", "fghi", "j"} {
		if count, err := writer.Write([]byte(data)); err != nil || count != len(data) {
			t.Fatalf("write = %d, %v", count, err)
		}
		if writer.content.Len() > writer.limit {
			t.Fatal("retained oversized content")
		}
	}
	if writer.size != 10 || writer.content.Len() != 0 {
		t.Fatalf("size = %d, content = %q", writer.size, writer.content.String())
	}
}

func viewImageFixture(width, height int) *image.NRGBA {
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x*37 + y*17), G: uint8(x*13 + y*47), B: uint8(x*71 + y*19), A: 255})
		}
	}
	return source
}

func encodeViewImageFixture(t testing.TB, source image.Image, format string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buffer, source)
	case "jpeg":
		err = jpeg.Encode(&buffer, source, &jpeg.Options{Quality: 90})
	case "gif":
		err = gif.Encode(&buffer, source, nil)
	case "bmp":
		err = bmp.Encode(&buffer, source)
	case "tiff":
		err = tiff.Encode(&buffer, source, nil)
	case "webp":
		// A 3x2 lossless WebP with red and green rows, generated with cwebp.
		data, decodeErr := base64.StdEncoding.DecodeString("UklGRh4AAABXRUJQVlA4TBIAAAAvAkAAAA+w//Mf8x8V5iCi/xE=")
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return data
	default:
		t.Fatalf("unknown fixture format %q", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func viewImageTestOperation(t *testing.T, data []byte, config ViewImageConfig) Operation {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.unknown")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := NewViewImageSpec(path, config)
	if err != nil {
		t.Fatal(err)
	}
	return Operation{ID: "view-image", Type: spec.Type, Version: spec.Version, State: spec.State, Status: StatusReady}
}

func runViewImageTest(t *testing.T, current Operation) (Operation, ViewImageResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	manager := NewLocalOperationManager(ctx)
	t.Cleanup(func() {
		cancel()
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case _, open := <-manager.Updates():
				if !open {
					return
				}
			case <-timeout.C:
				t.Fatal("manager did not stop and drain primitives")
			}
		}
	})
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case updated, open := <-manager.Updates():
			if !open {
				t.Fatal("manager stopped before image completed")
			}
			state, err := DecodeViewImageState(updated)
			if err != nil {
				t.Fatal(err)
			}
			if !localOperationFinished(updated.Status) {
				if state.Result != nil {
					t.Fatal("intermediate checkpoint contains an image result")
				}
				continue
			}
			if state.Result == nil {
				t.Fatal("terminal state is missing result")
			}
			return updated, *state.Result
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func decodeViewImageContent(t *testing.T, result ViewImageResult, wantFormat string) ([]byte, image.Image) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode output: %v (operation error: %s)", err, result.Error)
	}
	if format != wantFormat || result.EncodedMIMEType != "image/"+wantFormat {
		t.Fatalf("encoded format = %q, MIME = %q, want %s", format, result.EncodedMIMEType, wantFormat)
	}
	return data, decoded
}

func TestImageContextReaderAndWriterHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reader := &imageContextReader{ctx: ctx, reader: viewImageReader([][]byte{[]byte("image")})}
	if _, err := io.ReadAll(reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v", err)
	}
	writer := imageContentWriter{ctx: ctx, limit: 100}
	if _, err := writer.Write([]byte("image")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write error = %v", err)
	}
}
