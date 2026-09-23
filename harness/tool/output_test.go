package tool_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool/bash"
)

func TestOutputLengthMaximumMatchesToolSchemas(t *testing.T) {
	limit, err := tool.ParseMaxOutputLength(jsontext.Value(fmt.Sprint(operation.MaxOutputLength)))
	if err != nil || limit != operation.MaxOutputLength {
		t.Fatalf("maximum output length = %d, error = %v", limit, err)
	}
	registry := tool.NewRegistry(tool.StaticTranslators{}, tool.BashName)
	checked := 0
	for _, definition := range registry.StaticDefinitions() {
		if definition.Tool.Name != tool.BashName {
			continue
		}
		properties := definition.Tool.Parameters["properties"].(map[string]any)
		schema := properties["max_output_length"].(map[string]any)
		if schema["maximum"] != operation.MaxOutputLength || schema["default"] != operation.DefaultMaxOutputLength {
			t.Fatalf("%s output length schema = %#v", definition.Tool.Name, schema)
		}
		checked++
	}
	if checked != 1 {
		t.Fatalf("checked %d tool schemas, want 1", checked)
	}
}

func TestValidationErrorsAreBoundedBeforeResultTranslation(t *testing.T) {
	bashCall := bash.New(bash.Config{Shell: "/bin/sh", BaseDirectory: "/operations"})
	longName := strings.Repeat("é", operation.DefaultMaxOutputLength+1)
	malformed := fmt.Sprintf(`{%q:0,%q:0}`, longName, longName)
	for _, test := range []struct {
		name       string
		translator tool.Translator
		arguments  string
		limit      int
		truncated  bool
	}{
		{"Bash requested", bashCall, `{"command":null,"max_output_length":10}`, 10, true},
		{"Bash untruncated", bashCall, `{"command":null}`, operation.DefaultMaxOutputLength, false},
		{"Bash malformed", bashCall, malformed, operation.DefaultMaxOutputLength, true},
		{"Bash invalid limit", bashCall, fmt.Sprintf(`{"max_output_length":%q}`, longName), operation.DefaultMaxOutputLength, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := test.translator.Translate(nil, llm.ToolCall{Arguments: test.arguments})
			retained := regexp.MustCompile(`\.\.\.[0-9]+ bytes truncated\.\.\.`).ReplaceAllString(status.Error, "")
			if len(status.WaitingFor) != 0 || status.Error == "" || !utf8.ValidString(status.Error) || utf8.RuneCountInString(retained) > test.limit {
				t.Fatalf("status has %d error characters and %d operations, want at most %d characters and no operations", utf8.RuneCountInString(retained), len(status.WaitingFor), test.limit)
			}
			if status.ErrorTruncated != test.truncated {
				t.Fatalf("status error truncated = %t, want %t", status.ErrorTruncated, test.truncated)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			var restored tool.CallStatus
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			status = restored
			result, err := test.translator.TranslateResult("invalid-call", status, nil)
			if err != nil {
				t.Fatal(err)
			}
			output, found := strings.CutPrefix(result.Output[0].Value, "Error: ")
			if !found {
				t.Fatalf("Bash validation error has no error label: %q", result.Output[0].Value)
			}
			if output != status.Error {
				t.Fatal("result translation changed the prepared validation error")
			}
		})
	}
}

func TestToolsRejectInvalidOutputLengthsBeforeSubmission(t *testing.T) {
	for _, test := range []struct {
		name       string
		translator tool.Translator
		arguments  string
	}{
		{"Bash", bash.New(bash.Config{Shell: "/bin/sh", BaseDirectory: "/operations"}), `{"command":"pwd","max_output_length":`},
	} {
		for _, value := range []string{"0", "-1", fmt.Sprint(operation.MaxOutputLength + 1), "1000000000", "1.5", `"10"`, "true", "null", "[]", "{}", "999999999999999999999999"} {
			t.Run(test.name+"/"+value, func(t *testing.T) {
				status := test.translator.Translate(nil, llm.ToolCall{Arguments: test.arguments + value + "}"})
				if !strings.Contains(status.Error, "max_output_length") || len(status.WaitingFor) != 0 {
					t.Fatalf("status = %#v", status)
				}
				result, err := test.translator.TranslateResult("invalid-call", status, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(result.Output[0].Value, "max_output_length") {
					t.Fatalf("result = %#v", result)
				}
			})
		}
	}
}
