package operation_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
)

func TestNewRemoteJobSpecRoundTripsPlan(t *testing.T) {
	plan := operation.RemoteJobPlan{
		Type:    "test",
		Version: 2,
		Data:    jsontext.Value(`{"value":"preserved"}`),
	}
	spec, err := operation.NewRemoteJobSpec(plan)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != operation.TypeRemoteJob || spec.Version != operation.VersionRemoteJob {
		t.Fatalf("spec = %#v", spec)
	}
	state, err := operation.DecodeRemoteJobState(operation.Operation{
		ID:              "remote-1",
		Type:            spec.Type,
		Version:         spec.Version,
		Status:          operation.StatusReady,
		MaxOutputLength: spec.MaxOutputLength, State: spec.State,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Plan.Type != plan.Type || state.Plan.Version != plan.Version ||
		string(state.Plan.Data) != string(plan.Data) {
		t.Fatalf("state = %#v", state)
	}
}

func TestRemoteJobTerminalTransitionsClearSubscriptionAndTruncatedResult(t *testing.T) {
	plan := operation.RemoteJobPlan{
		Type:    "test",
		Version: 1,
		Data:    jsontext.Value(`{}`),
	}
	spec, err := operation.NewRemoteJobSpec(plan)
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{
		ID:              "remote-1",
		Type:            spec.Type,
		Version:         spec.Version,
		Status:          operation.StatusReady,
		MaxOutputLength: spec.MaxOutputLength, State: spec.State,
	}
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	state.Subscription = jsontext.Value(`{"partial":"response"}`)
	state.TerminalResult = `{"stale":true}`
	current.MaxOutputLength = 3
	awaiting, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := operation.DecodeRemoteJobState(*awaiting.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ResultTruncated || prepared.ResultBytes != len(state.TerminalResult) {
		t.Fatalf("awaiting result = %#v", prepared)
	}

	tests := []struct {
		name   string
		status operation.Status
		apply  func(operation.Operation) (operation.Step, error)
	}{
		{
			name:   "failed",
			status: operation.StatusFailed,
			apply: func(current operation.Operation) (operation.Step, error) {
				return operation.FailRemoteJob(current, errors.New("failed"))
			},
		},
		{
			name:   "canceled",
			status: operation.StatusCanceled,
			apply:  operation.CancelRemoteJob,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terminal, err := test.apply(*awaiting.Operation)
			if err != nil {
				t.Fatal(err)
			}
			state, err := operation.DecodeRemoteJobState(*terminal.Operation)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Operation.Status != test.status || len(state.Subscription) != 0 ||
				len(state.TerminalResult) != 0 || state.ResultBytes != 0 || state.ResultTruncated {
				t.Fatalf("operation = %#v, state = %#v", terminal.Operation, state)
			}
		})
	}
}

func TestFailRemoteJobReplacesTruncatedError(t *testing.T) {
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "remote", Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: 3}
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	state.TerminalError = "previous error"
	awaiting, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := operation.DecodeRemoteJobState(*awaiting.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ErrorTruncated || prepared.ErrorBytes != len(state.TerminalError) {
		t.Fatalf("previous error = %#v", prepared)
	}
	for _, test := range []struct {
		name      string
		message   string
		want      string
		truncated bool
	}{
		{name: "short", message: "é", want: "é"},
		{name: "oversized", message: "éééé", want: "é...2 bytes truncated...éé", truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failed, err := operation.FailRemoteJob(*awaiting.Operation, errors.New(test.message))
			if err != nil {
				t.Fatal(err)
			}
			state, err := operation.DecodeRemoteJobState(*failed.Operation)
			if err != nil {
				t.Fatal(err)
			}
			if state.TerminalError != test.want || state.ErrorBytes != len(test.message) || state.ErrorTruncated != test.truncated {
				t.Fatalf("replacement error = %#v", state)
			}
		})
	}
}

func TestNewRemoteJobSpecRejectsInvalidPlans(t *testing.T) {
	tests := []struct {
		name string
		plan operation.RemoteJobPlan
		want string
	}{
		{name: "missing type", plan: operation.RemoteJobPlan{Version: 1, Data: jsontext.Value(`{}`)}, want: "type must be set"},
		{name: "zero version", plan: operation.RemoteJobPlan{Type: "test", Data: jsontext.Value(`{}`)}, want: "version must be positive"},
		{name: "missing data", plan: operation.RemoteJobPlan{Type: "test", Version: 1}, want: "data must be valid JSON"},
		{name: "invalid data", plan: operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value(`{`)}, want: "data must be valid JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := operation.NewRemoteJobSpec(test.plan)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDecodeRemoteJobStateReportsUnsupportedEnvelope(t *testing.T) {
	for _, current := range []operation.Operation{
		{ID: "remote-1", Type: operation.TypeShell, Version: operation.VersionRemoteJob},
		{ID: "remote-1", Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob + 1},
	} {
		if _, err := operation.DecodeRemoteJobState(current); !errors.Is(err, operation.ErrUnsupported) {
			t.Fatalf("error = %v, want ErrUnsupported", err)
		}
	}
}

func TestRemoteJobPreparesOutputBeforePublishingUpdate(t *testing.T) {
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "remote", Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: 3}
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	state.TerminalResult = `"éééé"`
	step, err := operation.UpdateRemoteJob(current, state, operation.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	var prepared operation.RemoteJobState
	if err := json.Unmarshal(step.Operation.State, &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.TerminalResult != `"...6 bytes truncated...é"` || prepared.ResultBytes != 10 || !prepared.ResultTruncated {
		t.Fatalf("prepared result = %#v", prepared)
	}
	failed, err := operation.FailRemoteJob(current, errors.New("éééé"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(failed.Operation.State, &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.TerminalError != "é...2 bytes truncated...éé" || prepared.ErrorBytes != 8 || !prepared.ErrorTruncated {
		t.Fatalf("prepared error = %#v", prepared)
	}
}

func TestRemoteJobOutputRemainsTruncatedAfterRoundTrip(t *testing.T) {
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"result", "error"} {
		t.Run(name, func(t *testing.T) {
			failure := name == "error"
			current := operation.Operation{ID: "remote", Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: 3}
			state, err := operation.DecodeRemoteJobState(current)
			if err != nil {
				t.Fatal(err)
			}
			status := operation.StatusCompleted
			if failure {
				state.TerminalError = "éééé"
				status = operation.StatusFailed
			} else {
				state.TerminalResult = `"éééé"`
			}
			for range 2 {
				step, err := operation.UpdateRemoteJob(current, state, status)
				if err != nil {
					t.Fatal(err)
				}
				current = *step.Operation
				state, err = operation.DecodeRemoteJobState(current)
				if err != nil {
					t.Fatal(err)
				}
				if failure {
					if state.TerminalError != "é...2 bytes truncated...éé" || !state.ErrorTruncated || state.ErrorBytes != 8 {
						t.Fatalf("error after round trip = %#v", state)
					}
				} else if state.TerminalResult != `"...6 bytes truncated...é"` || !state.ResultTruncated || state.ResultBytes != 10 {
					t.Fatalf("result after round trip = %#v", state)
				}
			}
		})
	}
}

func TestRemoteJobRejectsInvalidOutputLimits(t *testing.T) {
	for _, limit := range []int{0, -1, operation.MaxOutputLength + 1, 1_000_000_000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := operation.DecodeRemoteJobState(operation.Operation{Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: limit}); err == nil {
				t.Fatal("remote job accepted an invalid limit")
			}
		})
	}
}

func TestRemoteJobAcceptsMaximumOutputLength(t *testing.T) {
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "test", Version: 1, Data: jsontext.Value(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operation.DecodeRemoteJobState(operation.Operation{Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: operation.MaxOutputLength}); err != nil {
		t.Fatal(err)
	}
}
