package inbox_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestInboxControlMessages(t *testing.T) {
	for _, mode := range []inbox.ControlMode{inbox.StopHard, inbox.StopWhenIdle, inbox.Heartbeat} {
		t.Run(string(mode), func(t *testing.T) {
			want := inbox.ControlMessage{Mode: mode, Reason: "stop now"}
			payload, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			input := inbox.Input{ID: "stop", Kind: inbox.InputControl, Payload: payload}
			got, err := submitAndReceive(t, newInbox(t), input).DecodeControlMessage()
			if err != nil || got != want {
				t.Fatalf("control message = %+v, error = %v", got, err)
			}
		})
	}
}

func TestInboxSettingsControls(t *testing.T) {
	for _, effort := range []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh, llm.ReasoningEffortXHigh, llm.ReasoningEffortMax} {
		t.Run(string(effort), func(t *testing.T) {
			want := inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: effort}}
			payload, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			input := inbox.Input{ID: "settings", Kind: inbox.InputControl, Payload: payload}
			got, err := submitAndReceive(t, newInbox(t), input).DecodeControlMessage()
			if err != nil || got != want {
				t.Fatalf("settings control = %+v, error = %v", got, err)
			}
		})
	}
}

func TestInboxRejectsInvalidControlMessages(t *testing.T) {
	for _, payload := range []string{
		"", "null", `{}`, `{"Mode":"unknown"}`, `{"Mode":"soft"}`, `{"Mode":42}`, `{"Mode":"hard"`,
		`{"Mode":"hard","extra":true}`,
		`{"Mode":"heartbeat","Reason":"waiting","extra":true}`,
		`{"Mode":"heartbeat"}`,
		`{"Mode":"heartbeat","Reason":""}`,
		`{"Mode":"heartbeat","Reason":null}`,
		`{"Mode":"settings"}`,
		`{"Mode":"settings","Parameters":null}`,
		`{"Mode":"settings","Parameters":{}}`,
		`{"Mode":"settings","Parameters":[]}`,
		`{"Mode":"settings","Parameters":"high"}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":""}}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":null}}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":"default"}}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":"turbo"}}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":42}}`,
		`{"Mode":"settings","Parameters":{"Model":"model"}}`,
		`{"Mode":"settings","Parameters":{"Model":"model","ReasoningEffort":"high"}}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":"high","extra":true}}`,
		`{"Mode":"settings","Parameters":{"ReasoningEffort":"high"},"extra":true}`,
		`{"Mode":"hard","Parameters":{"ReasoningEffort":"high"}}`,
		`{"Mode":"when_idle","Parameters":{"ReasoningEffort":"high"}}`,
		`{"Mode":"heartbeat","Reason":"waiting","Parameters":{"ReasoningEffort":"high"}}`,
		`{"Mode":"hard","Parameters":null}`,
	} {
		t.Run(payload, func(t *testing.T) {
			input := inbox.Input{ID: "stop", Kind: inbox.InputControl, Payload: jsontext.Value(payload)}
			if err := newInbox(t).Submit(t.Context(), input); err == nil {
				t.Fatal("invalid control message accepted")
			}
		})
	}
	if _, err := (inbox.Input{Kind: inbox.InputExternal, Payload: jsontext.Value(`{"Mode":"hard"}`)}).DecodeControlMessage(); err == nil {
		t.Fatal("external input decoded as control message")
	}
}
