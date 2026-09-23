package primitives_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestCreateFileCreatesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	request := primitives.IOCreateRequest{
		Source:        "operation-1",
		CorrelationID: "create-1",
		Kind:          primitives.IOCreateRegularFile,
		Path:          path,
		Mode:          primitives.IOCreateDefaultMode,
	}

	events := collectEvents(create(t.Context(), request))
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != primitives.PrimitiveEventIOCreateCompleted {
		t.Fatalf("event type = %q, want %q", event.Type, primitives.PrimitiveEventIOCreateCompleted)
	}
	if event.Source != request.Source || event.CorrelationID != request.CorrelationID {
		t.Fatalf("event identity = (%q, %q)", event.Source, event.CorrelationID)
	}
	result := eventResult[primitives.IOCreateResult](t, event)
	if result.Kind != primitives.IOCreateRegularFile {
		t.Fatalf("result = %#v", result)
	}

	created, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("created contents = %q, want empty", created)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&^0o664 != 0 {
		t.Fatalf("mode = %o, exceeds requested permissions 664", info.Mode().Perm())
	}
}

func TestCreateFileUsesRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	events := collectEvents(create(t.Context(), primitives.IOCreateRequest{
		Kind: primitives.IOCreateRegularFile,
		Path: path,
		Mode: 0o600,
	}))
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventIOCreateCompleted {
		t.Fatalf("events = %#v, want one completion", events)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCreateFileWithZeroModeIsReplayable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	request := primitives.IOCreateRequest{Kind: primitives.IOCreateRegularFile, Path: path}
	for range 2 {
		singleEvent(
			t,
			collectEvents(create(t.Context(), request)),
			primitives.PrimitiveEventIOCreateCompleted,
		)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0 {
		t.Fatalf("mode = %o, want 0", info.Mode().Perm())
	}
}

func TestCreateDirectoryCreatesEmptyDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	request := primitives.IOCreateRequest{
		Source:        "operation-1",
		CorrelationID: "create-1",
		Kind:          primitives.IOCreateDirectory,
		Path:          path,
		Mode:          0o700,
	}

	event := singleEvent(
		t,
		collectEvents(create(t.Context(), request)),
		primitives.PrimitiveEventIOCreateCompleted,
	)
	result := eventResult[primitives.IOCreateResult](t, event)
	if result.Kind != primitives.IOCreateDirectory {
		t.Fatalf("result = %#v", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = %v, want directory 0700", info.Mode())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory entries = %#v, want empty", entries)
	}
}

func TestCreateDirectoryAcceptsTrailingSeparator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	event := singleEvent(
		t,
		collectEvents(create(t.Context(), primitives.IOCreateRequest{
			Kind: primitives.IOCreateDirectory,
			Path: path + string(os.PathSeparator),
			Mode: 0o700,
		})),
		primitives.PrimitiveEventIOCreateCompleted,
	)
	if result := eventResult[primitives.IOCreateResult](t, event); result.Kind != primitives.IOCreateDirectory {
		t.Fatalf("result = %#v", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("created mode = %v, want directory", info.Mode())
	}
}

func TestCreateDirectoryWithZeroModeIsReplayable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	request := primitives.IOCreateRequest{Kind: primitives.IOCreateDirectory, Path: path}
	for range 2 {
		singleEvent(
			t,
			collectEvents(create(t.Context(), request)),
			primitives.PrimitiveEventIOCreateCompleted,
		)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0 {
		t.Fatalf("created mode = %v, want directory mode 0", info.Mode())
	}
}

func TestCreateUsesProcessUmask(t *testing.T) {
	parent := t.TempDir()
	oldUmask := syscall.Umask(0o027)
	defer syscall.Umask(oldUmask)

	for _, test := range []struct {
		request primitives.IOCreateRequest
		want    os.FileMode
	}{
		{
			request: primitives.IOCreateRequest{
				Kind: primitives.IOCreateRegularFile,
				Path: filepath.Join(parent, "file"),
				Mode: 0o666,
			},
			want: 0o640,
		},
		{
			request: primitives.IOCreateRequest{
				Kind: primitives.IOCreateDirectory,
				Path: filepath.Join(parent, "directory"),
				Mode: 0o777,
			},
			want: 0o750,
		},
	} {
		for range 2 {
			singleEvent(
				t,
				collectEvents(create(t.Context(), test.request)),
				primitives.PrimitiveEventIOCreateCompleted,
			)
		}
		info, err := os.Stat(test.request.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != test.want {
			t.Fatalf("mode of %q = %o, want %o", test.request.Path, info.Mode().Perm(), test.want)
		}
	}
}

func TestCreateAcceptsExistingPathWithRestrictiveModeWithoutChangingIt(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "directory")
	if err := os.Mkdir(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		request primitives.IOCreateRequest
		want    os.FileMode
	}{
		{request: primitives.IOCreateRequest{Kind: primitives.IOCreateRegularFile, Path: file, Mode: 0o600}, want: 0o400},
		{request: primitives.IOCreateRequest{Kind: primitives.IOCreateDirectory, Path: directory, Mode: 0o700}, want: 0o500},
	} {
		singleEvent(
			t,
			collectEvents(create(t.Context(), test.request)),
			primitives.PrimitiveEventIOCreateCompleted,
		)
		info, err := os.Stat(test.request.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != test.want {
			t.Fatalf("mode of %q = %o, want unchanged %o", test.request.Path, info.Mode().Perm(), test.want)
		}
	}
}

func TestCreateDirectoryPreservesPathResolution(t *testing.T) {
	directory := t.TempDir()
	separator := string(os.PathSeparator)
	path := directory + separator + "missing" + separator + ".." + separator + "created" + separator
	event := singleEvent(
		t,
		collectEvents(create(t.Context(), primitives.IOCreateRequest{
			Kind: primitives.IOCreateDirectory,
			Path: path,
			Mode: 0o700,
		})),
		primitives.PrimitiveEventFailed,
	)
	if failure := eventResult[primitives.PrimitiveFailureResult](t, event); failure.Error == "" {
		t.Fatal("failure error is empty")
	}
	if _, err := os.Stat(filepath.Join(directory, "created")); !os.IsNotExist(err) {
		t.Fatalf("stat cleaned target: %v, want not exist", err)
	}
}

func TestCreateRejectsUnsupportedKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	event := singleEvent(
		t,
		collectEvents(create(t.Context(), primitives.IOCreateRequest{Path: path})),
		primitives.PrimitiveEventFailed,
	)
	failure := eventResult[primitives.PrimitiveFailureResult](t, event)
	if failure.Error == "" {
		t.Fatal("failure error is empty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat rejected path: %v, want not exist", err)
	}
}

func TestCreateFilePreservesExistingFile(t *testing.T) {
	existing := []byte("existing contents")
	path := writeTestFile(t, existing)
	request := primitives.IOCreateRequest{
		Kind: primitives.IOCreateRegularFile,
		Path: path,
		Mode: 0o600,
	}

	event := singleEvent(
		t,
		collectEvents(create(t.Context(), request)),
		primitives.PrimitiveEventIOCreateCompleted,
	)
	if result := eventResult[primitives.IOCreateResult](t, event); result.Kind != request.Kind {
		t.Fatalf("result = %#v", result)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, existing) {
		t.Fatalf("contents = %q, want %q", contents, existing)
	}
}

func TestCreateDirectoryAllowsExistingDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	request := primitives.IOCreateRequest{Kind: primitives.IOCreateDirectory, Path: directory, Mode: 0o700}
	event := singleEvent(
		t,
		collectEvents(create(t.Context(), request)),
		primitives.PrimitiveEventIOCreateCompleted,
	)
	if result := eventResult[primitives.IOCreateResult](t, event); result.Kind != request.Kind {
		t.Fatalf("result = %#v", result)
	}
}

func TestCreateDirectoryConcurrentCallsAreIdempotent(t *testing.T) {
	const invocationCount = 16
	path := filepath.Join(t.TempDir(), "created")
	request := primitives.IOCreateRequest{
		Kind: primitives.IOCreateDirectory,
		Path: path,
		Mode: 0o700,
	}
	ctx := t.Context()
	start := make(chan struct{})
	results := make(chan []primitives.PrimitiveEvent, invocationCount)
	for range invocationCount {
		go func() {
			<-start
			results <- collectEvents(create(ctx, request))
		}()
	}
	close(start)

	for range invocationCount {
		event := singleEvent(t, <-results, primitives.PrimitiveEventIOCreateCompleted)
		if result := eventResult[primitives.IOCreateResult](t, event); result.Kind != primitives.IOCreateDirectory {
			t.Fatalf("result = %#v", result)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("created mode = %v, want directory", info.Mode())
	}
}

func TestCreateAcceptsExistingPathWithBroaderModeWithoutChangingIt(t *testing.T) {
	file := writeTestFile(t, nil)
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		request primitives.IOCreateRequest
		want    os.FileMode
	}{
		{request: primitives.IOCreateRequest{Kind: primitives.IOCreateRegularFile, Path: file, Mode: 0o600}, want: 0o644},
		{request: primitives.IOCreateRequest{Kind: primitives.IOCreateDirectory, Path: directory, Mode: 0o700}, want: 0o755},
	} {
		singleEvent(
			t,
			collectEvents(create(t.Context(), test.request)),
			primitives.PrimitiveEventIOCreateCompleted,
		)
		info, err := os.Stat(test.request.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != test.want {
			t.Fatalf("mode of %q = %o, want unchanged %o", test.request.Path, info.Mode().Perm(), test.want)
		}
	}
}

func TestCreateRejectsExistingPathOfWrongKind(t *testing.T) {
	for _, test := range []struct {
		request primitives.IOCreateRequest
		want    string
	}{
		{
			request: primitives.IOCreateRequest{Kind: primitives.IOCreateDirectory, Path: writeTestFile(t, nil), Mode: 0o700},
			want:    "not a directory",
		},
		{
			request: primitives.IOCreateRequest{Kind: primitives.IOCreateRegularFile, Path: t.TempDir(), Mode: 0o600},
			want:    "not a regular file",
		},
	} {
		event := singleEvent(
			t,
			collectEvents(create(t.Context(), test.request)),
			primitives.PrimitiveEventFailed,
		)
		if failure := eventResult[primitives.PrimitiveFailureResult](t, event); !strings.Contains(failure.Error, test.want) {
			t.Fatalf("failure = %q, want %q", failure.Error, test.want)
		}
	}
}

func TestCreateRejectsExistingSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "link")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	singleEvent(
		t,
		collectEvents(create(t.Context(), primitives.IOCreateRequest{
			Kind: primitives.IOCreateRegularFile,
			Path: path,
			Mode: 0o600,
		})),
		primitives.PrimitiveEventFailed,
	)
}

func TestCreateFileCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(t.TempDir(), "canceled")

	events := collectEvents(create(ctx, primitives.IOCreateRequest{
		Kind: primitives.IOCreateRegularFile,
		Path: path,
		Mode: primitives.IOCreateDefaultMode,
	}))
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventCanceled {
		t.Fatalf("events = %#v, want one cancellation", events)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat canceled path: %v, want not exist", err)
	}
}

func TestCreateFileFailureIsTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "created")
	request := primitives.IOCreateRequest{
		Source:        "operation-1",
		CorrelationID: "create-1",
		Kind:          primitives.IOCreateRegularFile,
		Path:          path,
		Mode:          primitives.IOCreateDefaultMode,
	}

	events := collectEvents(create(t.Context(), request))
	if len(events) != 1 || events[0].Type != primitives.PrimitiveEventFailed {
		t.Fatalf("events = %#v, want one failure", events)
	}
	result := eventResult[primitives.PrimitiveFailureResult](t, events[0])
	if result.Error == "" {
		t.Fatal("failure error is empty")
	}
	if events[0].Source != request.Source || events[0].CorrelationID != request.CorrelationID {
		t.Fatalf("event identity = (%q, %q)", events[0].Source, events[0].CorrelationID)
	}
}

func TestCreateFileConcurrentProcessesAreIdempotent(t *testing.T) {
	const processCount = 6
	directory := t.TempDir()
	path := filepath.Join(directory, "created")
	startReader, startWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	type childProcess struct {
		command *exec.Cmd
		stdout  bytes.Buffer
		stderr  bytes.Buffer
	}
	children := make([]childProcess, processCount)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for index := range children {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCreateFileProcessHelper$")
		command.Env = append(os.Environ(),
			"HARNESS_CREATE_FILE_HELPER=1",
			"HARNESS_CREATE_FILE_PATH="+path,
		)
		command.ExtraFiles = []*os.File{startReader, readyWriter}
		command.Stdout = &children[index].stdout
		command.Stderr = &children[index].stderr
		children[index].command = command
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if err := startReader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := readyWriter.Close(); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, processCount)
	if _, err := io.ReadFull(readyReader, ready); err != nil {
		t.Fatal(err)
	}
	if err := readyReader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := startWriter.Close(); err != nil {
		t.Fatal(err)
	}

	results := make([]createProcessResult, processCount)
	completedCount := 0
	failureCount := 0
	for index := range children {
		child := &children[index]
		if err := child.command.Wait(); err != nil {
			t.Fatalf(
				"child %d: %v\nstdout:\n%s\nstderr:\n%s",
				index,
				err,
				child.stdout.String(),
				child.stderr.String(),
			)
		}
		decoder := jsontext.NewDecoder(&child.stdout)
		if err := json.UnmarshalDecode(decoder, &results[index]); err != nil {
			t.Fatalf("decode child %d output %q: %v", index, child.stdout.String(), err)
		}
		if results[index].Completed {
			completedCount++
		}
		if results[index].Error != "" {
			failureCount++
		}
	}
	if completedCount != processCount {
		t.Fatalf("completed results = %d, want %d", completedCount, processCount)
	}
	if failureCount != 0 {
		t.Fatalf("failed results = %d, want 0", failureCount)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 0 {
		t.Fatalf("contents = %q, want empty", contents)
	}
	for _, result := range results {
		if !result.Completed || result.Kind != primitives.IOCreateRegularFile {
			t.Fatalf("child result = %#v", result)
		}
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("directory entries = %#v, want only created file", entries)
	}
}

type createProcessResult struct {
	Completed bool
	Kind      primitives.IOCreateKind
	Error     string
}

func TestCreateFileProcessHelper(t *testing.T) {
	if os.Getenv("HARNESS_CREATE_FILE_HELPER") != "1" {
		return
	}
	start := os.NewFile(3, "create-start")
	if start == nil {
		t.Fatal("start file is unavailable")
	}
	ready := os.NewFile(4, "create-ready")
	if ready == nil {
		t.Fatal("ready file is unavailable")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := ready.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, start); err != nil {
		t.Fatal(err)
	}
	if err := start.Close(); err != nil {
		t.Fatal(err)
	}

	request := primitives.IOCreateRequest{
		Kind: primitives.IOCreateRegularFile,
		Path: os.Getenv("HARNESS_CREATE_FILE_PATH"),
		Mode: 0o600,
	}
	events := collectEvents(create(t.Context(), request))
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}

	result := createProcessResult{}
	switch events[0].Type {
	case primitives.PrimitiveEventIOCreateCompleted:
		completed := eventResult[primitives.IOCreateResult](t, events[0])
		result.Completed = true
		result.Kind = completed.Kind
	case primitives.PrimitiveEventFailed:
		result.Error = eventResult[primitives.PrimitiveFailureResult](t, events[0]).Error
	default:
		result.Error = fmt.Sprintf("unexpected event type %q", events[0].Type)
	}
	if err := json.MarshalWrite(os.Stdout, result); err != nil {
		t.Fatal(err)
	}
}
