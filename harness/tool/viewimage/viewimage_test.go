package viewimage_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"image"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/bmp"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool/viewimage"
)

type recordingContext struct {
	specs []operation.Spec
}

func (ctx *recordingContext) Submit(spec operation.Spec) operation.ID {
	ctx.specs = append(ctx.specs, spec)
	return "image-operation"
}

func TestTranslatorSubmitsImageOperation(t *testing.T) {
	for _, test := range []struct {
		name      string
		path      string
		limits    operation.ViewImageConfig
		wantPath  string
		wantLimit operation.ViewImageConfig
	}{
		{"relative path", "image.png", operation.ViewImageConfig{}, "/workspace/image.png",
			operation.ViewImageConfig{MaxSize: viewimage.DefaultMaxSize, MaxWidth: viewimage.DefaultMaxWidth, MaxHeight: viewimage.DefaultMaxHeight}},
		{"absolute path", "/outside/image.png", operation.ViewImageConfig{MaxSize: 1000, MaxWidth: 20, MaxHeight: 10, MaxSourcePixels: 100}, "/outside/image.png",
			operation.ViewImageConfig{MaxSize: 1000, MaxWidth: 20, MaxHeight: 10, MaxSourcePixels: 100}},
		{"preserve path components", "link/../ image.png ", operation.ViewImageConfig{MaxWidth: 123}, "/workspace/link/../ image.png ",
			operation.ViewImageConfig{MaxSize: viewimage.DefaultMaxSize, MaxWidth: 123, MaxHeight: viewimage.DefaultMaxHeight}},
	} {
		t.Run(test.name, func(t *testing.T) {
			translator := viewimage.New(viewimage.Config{Directory: "/workspace", Limits: test.limits})
			ctx := &recordingContext{}
			arguments, err := json.Marshal(map[string]string{"path": test.path})
			if err != nil {
				t.Fatal(err)
			}
			status := translator.Translate(ctx, llm.ToolCall{Name: tool.ViewImageName, Arguments: string(arguments)})
			if status.Error != "" || !reflect.DeepEqual(status.WaitingFor, []operation.ID{"image-operation"}) || len(ctx.specs) != 1 {
				t.Fatalf("translation = %+v, specs = %+v", status, ctx.specs)
			}
			spec := ctx.specs[0]
			state, err := operation.DecodeViewImageState(operation.Operation{Type: spec.Type, Version: spec.Version, State: spec.State})
			if err != nil || state.Path != test.wantPath || state.Config != test.wantLimit || state.Result != nil {
				t.Fatalf("state = %+v, error = %v", state, err)
			}
		})
	}
}

func TestTranslatorRejectsInvalidArguments(t *testing.T) {
	translator := viewimage.New(viewimage.Config{})
	for _, arguments := range []string{"", "{", "null", "[]", "{}", `{"path":null}`, `{"path":false}`, `{"path":12}`, `{"path":""}`, `{"path":"  "}`, `{"path":"a\u0000b"}`} {
		t.Run(arguments, func(t *testing.T) {
			ctx := &recordingContext{}
			status := translator.Translate(ctx, llm.ToolCall{Arguments: arguments})
			if status.Error == "" || len(ctx.specs) != 0 || len(status.WaitingFor) != 0 {
				t.Fatalf("invalid arguments accepted: %+v, specs = %+v", status, ctx.specs)
			}
			result, err := translator.TranslateResult("call", status, nil)
			if err != nil || result.CallID != "call" || len(result.Output) != 1 || result.Output[0].Kind != llm.ToolResultText ||
				!strings.Contains(result.Output[0].Value, status.Error) {
				t.Fatalf("validation result = %+v, error = %v", result, err)
			}
		})
	}
	ctx := &recordingContext{}
	status := viewimage.New(viewimage.Config{Limits: operation.ViewImageConfig{MaxSize: -1}}).Translate(ctx, llm.ToolCall{Arguments: `{"path":"image.png"}`})
	if status.Error == "" || len(ctx.specs) != 0 {
		t.Fatalf("invalid limits accepted: %+v", status)
	}
}

func TestTranslatorFormatsConditionalImageMetadata(t *testing.T) {
	for _, test := range []struct {
		name     string
		original string
		encoded  string
		ratio    float64
		text     string
	}{
		{"unchanged PNG", "image/png", "image/png", 1, ""},
		{"unchanged JPEG", "image/jpeg", "image/jpeg", 1, ""},
		{"converted only", "image/bmp", "image/png", 1, "original MIME type: image/bmp"},
		{"resized only", "image/jpeg", "image/jpeg", 0.25, "original dimensions: 120x80; multiply coordinates by 4.00 to approximate original"},
		{"converted and resized", "image/tiff", "image/png", 0.5, "original MIME type: image/tiff; original dimensions: 120x80; multiply coordinates by 2.00 to approximate original"},
		{"fractional coordinates", "image/png", "image/png", 1 / 3.25, "original dimensions: 120x80; multiply coordinates by 3.25 to approximate original"},
		{"rounded coordinates", "image/png", "image/png", 1 / 3.456, "original dimensions: 120x80; multiply coordinates by 3.46 to approximate original"},
		{"large coordinates", "image/png", "image/png", 1.0 / 120, "original dimensions: 120x80; multiply coordinates by 120.00 to approximate original"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := imageOperation(t, operation.StatusCompleted, &operation.ViewImageResult{
				Content: "aW1hZ2U=", OriginalWidth: 120, OriginalHeight: 80,
				OriginalMIMEType: test.original, EncodedMIMEType: test.encoded, ScaleRatio: test.ratio,
			})
			result, err := viewimage.New(viewimage.Config{}).TranslateResult("call", tool.CallStatus{}, []operation.Operation{current})
			want := llm.ToolResult{CallID: "call", Output: []llm.ToolResultOutput{
				{Kind: llm.ToolResultImage, Value: "data:" + test.encoded + ";base64,aW1hZ2U="},
			}}
			if test.text != "" {
				want.Output = append(want.Output, llm.ToolResultOutput{Kind: llm.ToolResultText, Value: test.text})
			}
			if err != nil || !reflect.DeepEqual(result, want) {
				t.Fatalf("result = %+v, error = %v, want %+v", result, err, want)
			}
		})
	}
}

func TestTranslatorFormatsFailuresAndPendingOperations(t *testing.T) {
	for _, test := range []struct {
		name   string
		status operation.Status
		result *operation.ViewImageResult
		want   string
	}{
		{"ready", operation.StatusReady, nil, "Image is still loading."},
		{"awaiting", operation.StatusAwaiting, nil, "Image is still loading."},
		{"canceling", operation.StatusCanceling, nil, "Image is still loading."},
		{"failed before header", operation.StatusFailed, &operation.ViewImageResult{Error: "not an image"}, "Error: not an image"},
		{"failed after header", operation.StatusFailed, &operation.ViewImageResult{
			Content: "must not return this", OriginalWidth: 120, OriginalHeight: 80, OriginalMIMEType: "image/png", EncodedMIMEType: "image/png", ScaleRatio: 1,
			Error: "needs 1200 base64 bytes; MaxSize is 1000 bytes, exceeded by 200 bytes",
		}, "Error: needs 1200 base64 bytes; MaxSize is 1000 bytes, exceeded by 200 bytes; original MIME type: image/png; original dimensions: 120x80"},
		{"canceled", operation.StatusCanceled, &operation.ViewImageResult{Error: "canceled while reading"}, "Error: canceled while reading"},
		{"canceled without result", operation.StatusCanceled, nil, "Error: view-image operation canceled"},
		{"failed without result", operation.StatusFailed, nil, "Error: view-image operation failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := viewimage.New(viewimage.Config{}).TranslateResult("call", tool.CallStatus{}, []operation.Operation{imageOperation(t, test.status, test.result)})
			want := llm.ToolResult{CallID: "call", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: test.want}}}
			if err != nil || !reflect.DeepEqual(result, want) {
				t.Fatalf("result = %+v, error = %v, want %+v", result, err, want)
			}
		})
	}
}

func TestTranslatorRejectsInvalidOperationResults(t *testing.T) {
	valid := imageOperation(t, operation.StatusCompleted, &operation.ViewImageResult{Content: "aW1hZ2U=", EncodedMIMEType: "image/png", ScaleRatio: 1})
	wrongType, malformed, unknownStatus := valid, valid, valid
	wrongType.Type = operation.TypeShell
	malformed.State = []byte("{")
	unknownStatus.Status = "unknown"
	for _, test := range []struct {
		name       string
		status     tool.CallStatus
		operations []operation.Operation
	}{
		{"no operations", tool.CallStatus{}, nil},
		{"multiple operations", tool.CallStatus{}, []operation.Operation{valid, valid}},
		{"validation error with operation", tool.CallStatus{Error: "invalid"}, []operation.Operation{valid}},
		{"wrong type", tool.CallStatus{}, []operation.Operation{wrongType}},
		{"malformed state", tool.CallStatus{}, []operation.Operation{malformed}},
		{"unknown status", tool.CallStatus{}, []operation.Operation{unknownStatus}},
		{"missing result", tool.CallStatus{}, []operation.Operation{imageOperation(t, operation.StatusCompleted, nil)}},
		{"missing content", tool.CallStatus{}, []operation.Operation{imageOperation(t, operation.StatusCompleted, &operation.ViewImageResult{EncodedMIMEType: "image/png", ScaleRatio: 1})}},
		{"invalid ratio", tool.CallStatus{}, []operation.Operation{imageOperation(t, operation.StatusCompleted, &operation.ViewImageResult{Content: "data", EncodedMIMEType: "image/png"})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := viewimage.New(viewimage.Config{}).TranslateResult("call", test.status, test.operations); err == nil {
				t.Fatal("invalid operation result accepted")
			}
		})
	}
}

func TestViewImageReadsWorkspaceFileAndReturnsResizedDataURL(t *testing.T) {
	directory := t.TempDir()
	var source bytes.Buffer
	if err := bmp.Encode(&source, image.NewNRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "image.bmp"), source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	translator := viewimage.New(viewimage.Config{Directory: directory, Limits: operation.ViewImageConfig{MaxWidth: 6, MaxHeight: 6}})
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: translator}, tool.ViewImageName)
	definitions := registry.StaticDefinitions()
	if len(definitions) != 1 || definitions[0].Tool.Name != tool.ViewImageName ||
		!reflect.DeepEqual(definitions[0].Tool.Parameters["required"], []any{"path"}) ||
		len(definitions[0].Tool.Parameters["properties"].(map[string]any)) != 1 {
		t.Fatalf("ViewImage definition = %+v", definitions)
	}
	resolved, ok := registry.Resolve(tool.ViewImageName)
	if !ok {
		t.Fatal("ViewImage is not registered")
	}
	ctx := &recordingContext{}
	status := resolved.Translate(ctx, llm.ToolCall{Name: tool.ViewImageName, Arguments: `{"path":"image.bmp"}`})
	if status.Error != "" || len(ctx.specs) != 1 {
		t.Fatalf("translation = %+v, specs = %+v", status, ctx.specs)
	}
	managerCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	manager := operation.NewLocalOperationManager(managerCtx)
	t.Cleanup(func() {
		cancel()
		for range manager.Updates() {
		}
	})
	spec := ctx.specs[0]
	if err := manager.Add(operation.Operation{ID: status.WaitingFor[0], Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusReady}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case current, open := <-manager.Updates():
			if !open {
				t.Fatal("operation manager stopped before completing the image")
			}
			if current.Status == operation.StatusReady || current.Status == operation.StatusAwaiting {
				continue
			}
			result, err := resolved.TranslateResult("call", status, []operation.Operation{current})
			if err != nil || current.Status != operation.StatusCompleted || len(result.Output) != 2 || result.Output[0].Kind != llm.ToolResultImage {
				t.Fatalf("result = %+v, status = %s, error = %v", result, current.Status, err)
			}
			encoded, found := strings.CutPrefix(result.Output[0].Value, "data:image/png;base64,")
			if !found {
				t.Fatalf("invalid image data URL: %q", result.Output[0].Value)
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			decoded, format, err := image.Decode(bytes.NewReader(data))
			if err != nil || format != "png" || decoded.Bounds() != image.Rect(0, 0, 6, 4) {
				t.Fatalf("decoded image = %+v, format = %s, error = %v", decoded, format, err)
			}
			if result.Output[1].Kind != llm.ToolResultText || result.Output[1].Value != "original MIME type: image/bmp; original dimensions: 12x8; multiply coordinates by 2.00 to approximate original" {
				t.Fatalf("metadata = %+v", result.Output[1])
			}
			return
		case <-managerCtx.Done():
			t.Fatal(managerCtx.Err())
		}
	}
}

func imageOperation(t *testing.T, status operation.Status, result *operation.ViewImageResult) operation.Operation {
	t.Helper()
	encoded, err := json.Marshal(operation.ViewImageState{
		Path: "image.png", Config: operation.ViewImageConfig{MaxSize: 1000, MaxWidth: 30, MaxHeight: 20}, Result: result,
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: "image-operation", Type: operation.TypeViewImage, Version: operation.VersionViewImage, Status: status, State: encoded}
}
