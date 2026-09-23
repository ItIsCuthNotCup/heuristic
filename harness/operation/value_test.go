package operation_test

import (
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

func TestValueSpecCarriesInlineJSONResult(t *testing.T) {
	want := jsontext.Value(`{"tools":[{"name":"search"}]}`)
	spec, err := operation.NewValueSpec(want)
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{
		ID:      "value-1",
		Type:    spec.Type,
		Version: spec.Version,
		Status:  operation.StatusReady,
		State:   spec.State,
	}
	got, err := operation.DecodeValue(current)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("value = %s, want %s", got, want)
	}

	want[0] = '['
	got[0] = '['
	decodedAgain, err := operation.DecodeValue(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(decodedAgain) != `{"tools":[{"name":"search"}]}` {
		t.Fatalf("stored value changed to %s", decodedAgain)
	}
}

func TestNewValueSpecRejectsInvalidJSON(t *testing.T) {
	for _, value := range []jsontext.Value{nil, jsontext.Value(`{`)} {
		if _, err := operation.NewValueSpec(value); err == nil ||
			err.Error() != "value operation result must be valid JSON" {
			t.Fatalf("value %q error = %v", value, err)
		}
	}
}

func TestAdvanceValueCompletesWithoutPrimitiveDispatch(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"result":42}`))
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{
		ID: "value-1", Type: spec.Type, Version: spec.Version,
		Status: operation.StatusReady, State: spec.State,
	}
	step, err := operation.AdvanceValue(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if step.Operation.Status != operation.StatusCompleted || len(step.Dispatches) != 0 {
		t.Fatalf("step = %#v", step)
	}
	canceling := current
	canceling.Status = operation.StatusCanceling
	canceled, err := operation.AdvanceValue(canceling, nil)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Operation.Status != operation.StatusCanceled || len(canceled.Dispatches) != 0 {
		t.Fatalf("canceled step = %#v", canceled)
	}

	if _, err := operation.AdvanceValue(current, &primitives.PrimitiveEvent{}); err == nil ||
		!strings.Contains(err.Error(), "unexpected primitive event") {
		t.Fatalf("primitive event error = %v", err)
	}

	awaiting := current
	awaiting.Status = operation.StatusAwaiting
	if _, err := operation.AdvanceValue(awaiting, nil); !errors.Is(err, operation.ErrUnsupported) {
		t.Fatalf("awaiting status error = %v, want ErrUnsupported", err)
	}
}

func TestLocalOperationManagerCompletesValue(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"result":42}`))
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{
		ID: "value-1", Type: spec.Type, Version: spec.Version,
		Status: operation.StatusReady, State: spec.State,
	}
	manager := operation.NewLocalOperationManager(t.Context())
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	completed := <-manager.Updates()
	if completed.Status != operation.StatusCompleted {
		t.Fatalf("operation = %#v", completed)
	}
	value, err := operation.DecodeValue(completed)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != `{"result":42}` {
		t.Fatalf("value = %s", value)
	}
}

func TestDecodeValueRejectsOtherTypesAndVersions(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`null`))
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []operation.Operation{
		{ID: "wrong-type", Type: operation.TypeShell, Version: spec.Version, State: spec.State},
		{ID: "wrong-version", Type: spec.Type, Version: spec.Version + 1, State: spec.State},
	} {
		if _, err := operation.DecodeValue(current); !errors.Is(err, operation.ErrUnsupported) {
			t.Fatalf("operation %#v error = %v, want ErrUnsupported", current, err)
		}
	}
}

func TestDecodeAndAdvanceValueRejectMalformedState(t *testing.T) {
	for _, state := range []jsontext.Value{
		jsontext.Value(`[]`),
		jsontext.Value(`{}`),
		jsontext.Value(`{"Value":`),
	} {
		current := operation.Operation{
			ID: "value-1", Type: operation.TypeValue, Version: operation.VersionValue,
			Status: operation.StatusReady, State: state,
		}
		if _, err := operation.DecodeValue(current); err == nil ||
			!strings.Contains(err.Error(), `decode value operation "value-1" state`) {
			t.Fatalf("state %q decode error = %v", state, err)
		}
		if _, err := operation.AdvanceValue(current, nil); err == nil ||
			!strings.Contains(err.Error(), `decode value operation "value-1" state`) {
			t.Fatalf("state %q advance error = %v", state, err)
		}
	}
}

func TestLocalOperationManagerDeliversCanceledValue(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"result":42}`))
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{
		ID: "value-1", Type: spec.Type, Version: spec.Version,
		Status: operation.StatusCanceling, State: spec.State,
	}
	manager := operation.NewLocalOperationManager(t.Context())
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	canceled := <-manager.Updates()
	if canceled.Status != operation.StatusCanceled {
		t.Fatalf("operation = %#v", canceled)
	}
	value, err := operation.DecodeValue(canceled)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != `{"result":42}` {
		t.Fatalf("value = %s", value)
	}
}
