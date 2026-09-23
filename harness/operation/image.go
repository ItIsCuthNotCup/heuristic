package operation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

const (
	TypeViewImage    Type    = "view_image"
	VersionViewImage Version = 1

	DefaultMaxViewImageSourcePixels = 32_000_000
	MaxViewImageSourceBytes         = 256 * 1024 * 1024

	viewImageReadCorrelation    primitives.CorrelationID = "view-image-read"
	viewImageProcessCorrelation primitives.CorrelationID = "view-image-process"
)

type ViewImageConfig struct {
	// MaxSize limits the base64 content in bytes, excluding any data URL prefix.
	MaxSize   int
	MaxHeight int
	MaxWidth  int

	// MaxSourcePixels bounds decoding work; zero uses DefaultMaxViewImageSourcePixels.
	MaxSourcePixels int `json:",omitzero"`
}

type ViewImageResult struct {
	Content          string
	OriginalWidth    int
	OriginalHeight   int
	OriginalMIMEType string
	EncodedMIMEType  string
	ScaleRatio       float64
	Error            string
}

type ViewImageState struct {
	Path   string
	Config ViewImageConfig
	Result *ViewImageResult
}

type ViewImage struct {
	current    Operation
	state      ViewImageState
	chunks     [][]byte
	readSize   int64
	processing bool
}

func NewViewImageSpec(path string, config ViewImageConfig) (Spec, error) {
	state := ViewImageState{Path: path, Config: config}
	if err := validateViewImageState(state); err != nil {
		return Spec{}, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return Spec{}, fmt.Errorf("encode view-image operation state: %w", err)
	}
	return Spec{Type: TypeViewImage, Version: VersionViewImage, State: encoded}, nil
}

func NewViewImage(current Operation) (*ViewImage, error) {
	state, err := DecodeViewImageState(current)
	if err != nil {
		return nil, err
	}
	current.State = nil
	return &ViewImage{current: current, state: state}, nil
}

func DecodeViewImageState(current Operation) (ViewImageState, error) {
	if current.Type != TypeViewImage || current.Version != VersionViewImage {
		return ViewImageState{}, fmt.Errorf(
			"decode view-image operation %q: type %q version %d: %w",
			current.ID, current.Type, current.Version, ErrUnsupported,
		)
	}
	var state ViewImageState
	if err := json.Unmarshal(current.State, &state); err != nil {
		return ViewImageState{}, fmt.Errorf("decode view-image operation %q state: %w", current.ID, err)
	}
	if err := validateViewImageState(state); err != nil {
		return ViewImageState{}, fmt.Errorf("validate view-image operation %q state: %w", current.ID, err)
	}
	return state, nil
}

func failViewImage(current Operation, cause error) (Step, error) {
	view, err := NewViewImage(current)
	if err != nil {
		return Step{}, err
	}
	return view.fail(cause)
}

func validateViewImageState(state ViewImageState) error {
	if state.Path == "" {
		return errors.New("image path must be set")
	}
	if state.Config.MaxSize <= 0 || state.Config.MaxHeight <= 0 || state.Config.MaxWidth <= 0 {
		return errors.New("image MaxSize, MaxHeight, and MaxWidth must be positive")
	}
	if state.Config.MaxSourcePixels < 0 {
		return errors.New("image MaxSourcePixels must not be negative")
	}
	return nil
}

func (view *ViewImage) Handle(event *primitives.PrimitiveEvent) (Step, error) {
	switch view.current.Status {
	case StatusReady, StatusAwaiting:
		if view.state.Result != nil {
			return Step{}, fmt.Errorf("advance view-image operation %q: state is terminal", view.current.ID)
		}
		if event == nil {
			if view.processing {
				return Step{}, fmt.Errorf("advance view-image operation %q: processing is already in progress", view.current.ID)
			}
			return view.read()
		}
		if view.current.Status == StatusReady {
			return Step{}, fmt.Errorf("advance ready view-image operation %q: unexpected primitive event", view.current.ID)
		}
		return view.handleEvent(*event)
	case StatusCanceling:
		if event != nil {
			return Step{}, fmt.Errorf("advance canceling view-image operation %q: unexpected primitive event", view.current.ID)
		}
		return view.cancel()
	default:
		return Step{}, fmt.Errorf("advance view-image operation %q: terminal status %q", view.current.ID, view.current.Status)
	}
}

func (view *ViewImage) read() (Step, error) {
	view.chunks, view.readSize = nil, 0
	view.current.Status = StatusAwaiting
	step, err := view.checkpoint()
	if err != nil {
		return Step{}, err
	}
	step.Dispatches = []PrimitiveDispatch{{
		Type: primitives.PrimitiveDispatchIORead,
		Data: primitives.IOReadRequest{
			Source:        primitives.SourceID(view.current.ID),
			CorrelationID: viewImageReadCorrelation,
			Path:          view.state.Path,
			Count:         MaxViewImageSourceBytes,
		},
	}}
	return step, nil
}

func (view *ViewImage) handleEvent(event primitives.PrimitiveEvent) (Step, error) {
	if event.Source != primitives.SourceID(view.current.ID) {
		return Step{}, fmt.Errorf("image event source is %q, want %q", event.Source, view.current.ID)
	}
	correlation := viewImageReadCorrelation
	if view.processing {
		correlation = viewImageProcessCorrelation
	}
	// Dispatch rejection has no correlation because the primitive never started.
	dispatchRejected := event.Type == primitives.PrimitiveEventFailed && event.CorrelationID == ""
	if event.CorrelationID != correlation && !dispatchRejected {
		return Step{}, fmt.Errorf("image event correlation is %q, want %q", event.CorrelationID, correlation)
	}
	if event.Type == primitives.PrimitiveEventCanceled {
		if event.Result != nil {
			return Step{}, errors.New("image cancellation returned an invalid result")
		}
		return view.cancel()
	}
	if event.Type == primitives.PrimitiveEventFailed {
		failure, ok := event.Result.(primitives.PrimitiveFailureResult)
		if !ok {
			return Step{}, errors.New("image primitive failed with an invalid result")
		}
		if view.processing || dispatchRejected {
			return view.fail(fmt.Errorf("image primitive failed: %s", failure.Error))
		}
		cause := fmt.Errorf("read image: %s", failure.Error)
		if view.readSize == 0 {
			return view.fail(cause)
		}
		return view.process(cause), nil
	}
	if view.processing {
		return view.processed(event)
	}
	return view.handleRead(event)
}

func (view *ViewImage) handleRead(event primitives.PrimitiveEvent) (Step, error) {
	switch event.Type {
	case primitives.PrimitiveEventIOReadOutput:
		output, ok := event.Result.(primitives.IOReadOutputResult)
		if !ok || output.Offset != view.readSize || int64(len(output.Data)) > MaxViewImageSourceBytes-view.readSize {
			return Step{}, errors.New("image read returned invalid output")
		}
		view.chunks = append(view.chunks, output.Data)
		view.readSize += int64(len(output.Data))
		return Step{}, nil
	case primitives.PrimitiveEventIOReadCompleted:
		completed, ok := event.Result.(primitives.IOReadCompletedResult)
		if !ok || completed.Size < 0 || min(completed.Size, MaxViewImageSourceBytes) != view.readSize {
			return Step{}, errors.New("image read returned an invalid completion")
		}
		if completed.Size > MaxViewImageSourceBytes {
			return view.process(fmt.Errorf("image source is %d bytes; source limit is %d bytes (exceeded by %d bytes)",
				completed.Size, MaxViewImageSourceBytes, completed.Size-MaxViewImageSourceBytes)), nil
		}
		return view.process(nil), nil
	default:
		return Step{}, fmt.Errorf("image read returned unexpected event %q", event.Type)
	}
}

func inspectViewImage(reader io.Reader) (ViewImageResult, string, error) {
	result := ViewImageResult{ScaleRatio: 1}
	config, format, err := image.DecodeConfig(reader)
	if err != nil {
		return result, format, fmt.Errorf("decode image header; original dimensions unavailable: %w", err)
	}
	result.OriginalWidth, result.OriginalHeight = config.Width, config.Height
	if config.Width <= 0 || config.Height <= 0 {
		return result, format, errors.New("image dimensions must be positive")
	}
	switch format {
	case "gif":
		result.OriginalMIMEType = "image/gif"
		return result, format, errors.New("unsupported image format \"gif\"")
	case "jpeg", "png", "bmp", "tiff", "webp":
		result.OriginalMIMEType = "image/" + format
	default:
		return result, format, fmt.Errorf("unsupported image format %q", format)
	}
	return result, format, nil
}

func prepareViewImage(ctx context.Context, chunks [][]byte, config ViewImageConfig, cause error) (ViewImageResult, error) {
	// Inspect the buffered header even when reading failed.
	result, format, err := inspectViewImage(viewImageReader(chunks))
	if cause != nil || err != nil {
		return result, errors.Join(cause, err)
	}
	maxPixels := config.MaxSourcePixels
	if maxPixels == 0 {
		maxPixels = DefaultMaxViewImageSourcePixels
	}
	if result.OriginalWidth > maxPixels/result.OriginalHeight {
		return result, fmt.Errorf("refuse to decode %dx%d %s image: exceeds MaxSourcePixels %d",
			result.OriginalWidth, result.OriginalHeight, format, maxPixels)
	}
	source, _, err := image.Decode(&imageContextReader{ctx: ctx, reader: viewImageReader(chunks)})
	if err != nil {
		return result, fmt.Errorf("decode %s pixels: %w", format, err)
	}
	width, height, ratio := viewImageDimensions(result.OriginalWidth, result.OriginalHeight, config)
	result.ScaleRatio = ratio
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if ratio < 1 {
		resized := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.BiLinear.Scale(resized, resized.Bounds(), source, source.Bounds(), draw.Src, nil)
		source = resized
	}

	result.EncodedMIMEType = "image/png"
	if format == "jpeg" {
		result.EncodedMIMEType = "image/jpeg"
	}
	content := imageContentWriter{ctx: ctx, limit: config.MaxSize}
	encoder := base64.NewEncoder(base64.StdEncoding, &content)
	switch {
	case format == "jpeg" && ratio == 1:
		_, err = io.Copy(encoder, viewImageReader(chunks))
	case format == "jpeg":
		err = jpeg.Encode(encoder, source, &jpeg.Options{Quality: 90})
	default:
		err = png.Encode(encoder, source)
	}
	err = errors.Join(err, encoder.Close(), ctx.Err())
	if err != nil {
		return result, fmt.Errorf("encode %s: %w; produced %d base64 bytes before stopping, final size unavailable (MaxSize %d bytes)",
			result.EncodedMIMEType, err, content.size, config.MaxSize)
	}

	if content.size > int64(config.MaxSize) {
		return result, fmt.Errorf(
			"%s image after resizing %dx%d to %dx%d (scale %.6g) needs %d base64 bytes; MaxSize is %d bytes, exceeded by %d bytes (%.1f%%)",
			result.EncodedMIMEType, result.OriginalWidth, result.OriginalHeight, width, height, ratio,
			content.size, config.MaxSize, content.size-int64(config.MaxSize), 100*float64(content.size-int64(config.MaxSize))/float64(config.MaxSize),
		)
	}
	result.Content = content.content.String()
	return result, nil
}

func viewImageDimensions(width, height int, config ViewImageConfig) (int, int, float64) {
	ratio := min(1, float64(config.MaxWidth)/float64(width), float64(config.MaxHeight)/float64(height))
	return max(1, int(math.Round(float64(width)*ratio))), max(1, int(math.Round(float64(height)*ratio))), ratio
}

func (view *ViewImage) fail(err error) (Step, error) {
	return view.finish(ViewImageResult{
		ScaleRatio: 1, Error: err.Error(),
	}, StatusFailed)
}

func (view *ViewImage) cancel() (Step, error) {
	return view.finish(ViewImageResult{
		ScaleRatio: 1, Error: fmt.Sprintf("view-image operation canceled: %v", context.Canceled),
	}, StatusCanceled)
}

func (view *ViewImage) checkpoint() (Step, error) {
	encoded, err := json.Marshal(view.state)
	if err != nil {
		return Step{}, fmt.Errorf("encode view-image operation %q state: %w", view.current.ID, err)
	}
	current := view.current
	current.State = encoded
	return Step{Operation: &current}, nil
}

func (view *ViewImage) process(cause error) Step {
	source, config, chunks := primitives.SourceID(view.current.ID), view.state.Config, view.chunks
	view.chunks, view.readSize = nil, 0
	view.processing = true
	return Step{Dispatches: []PrimitiveDispatch{{
		Type: primitives.PrimitiveDispatchCompute,
		Data: primitives.ComputeRequest{
			Source: source, CorrelationID: viewImageProcessCorrelation,
			Run: func(ctx context.Context) (any, error) {
				result, err := prepareViewImage(ctx, chunks, config, cause)
				if err != nil {
					result.Content = ""
					result.Error = err.Error()
				}
				// Image failures are values so compute preserves their diagnostic metadata.
				return result, nil
			},
		},
	}}}
}

func (view *ViewImage) processed(event primitives.PrimitiveEvent) (Step, error) {
	if event.Type != primitives.PrimitiveEventComputeCompleted {
		return Step{}, fmt.Errorf("image processing returned unexpected event %q", event.Type)
	}
	computed, ok := event.Result.(primitives.ComputeResult)
	if !ok {
		return Step{}, errors.New("image processing returned an invalid compute result")
	}
	result, ok := computed.Value.(ViewImageResult)
	if !ok {
		return Step{}, errors.New("image processing returned an invalid image result")
	}
	status := StatusCompleted
	if result.Error != "" {
		result.Content = ""
		status = StatusFailed
	}
	return view.finish(result, status)
}

func (view *ViewImage) finish(result ViewImageResult, status Status) (Step, error) {
	view.state.Result = &result
	view.current.Status = status
	view.chunks, view.readSize = nil, 0
	view.processing = false
	return view.checkpoint()
}

func viewImageReader(chunks [][]byte) io.Reader {
	readers := make([]io.Reader, len(chunks))
	for index, chunk := range chunks {
		readers[index] = bytes.NewReader(chunk)
	}
	return io.MultiReader(readers...)
}

type imageContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *imageContextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

type imageContentWriter struct {
	ctx     context.Context
	limit   int
	size    int64
	content strings.Builder
}

func (writer *imageContentWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	writer.size += int64(len(data))
	if writer.size > int64(writer.limit) {
		// Keep counting to report the exact excess without retaining oversized output.
		writer.content.Reset()
		return len(data), nil
	}
	return writer.content.Write(data)
}
