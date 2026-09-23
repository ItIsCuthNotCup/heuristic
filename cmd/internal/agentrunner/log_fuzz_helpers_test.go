package agentrunner

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore/localfile"
)

func addLogFuzzSeeds(f *testing.F) {
	f.Helper()
	for _, seed := range []struct {
		actions []byte
		text    string
		tokens  uint64
	}{
		{nil, "", 0},
		{[]byte{0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 3, 13, 14, 15}, "quotes\"\\\n\r\t\x00 会話\u2028", 1},
		{[]byte{3, 4, 8, 5, 8, 6, 8, 3, 9}, "precision", 1<<53 + 1},
		{[]byte{10, 3, 9, 8, 11, 8, 3}, "\xffinvalid UTF-8", math.MaxInt64},
		{[]byte{4, 6, 6, 6, 6, 6, 8, 3}, strings.Repeat("large\n", 200), 1000000},
		{[]byte{1, 2, 3, 4, 8, 3}, "provider error and resume", 12345},
		{[]byte{0, 1, 2, 0, 1}, "hard stop during request", 9},
		{[]byte{0, 1, 2, 0, 2}, "cancel during request", 17},
		{[]byte{0, 3}, strings.Repeat("x", 70<<10), 42},
	} {
		f.Add(seed.actions, seed.text, seed.tokens, seed.tokens/3, seed.tokens/5, seed.tokens, seed.tokens/2)
	}
}

func fuzzLogUsage(t *testing.T, text string, input, cached, written, output, reasoning uint64) llm.Usage {
	t.Helper()
	return llm.Usage{
		InputTokens: int64(input & math.MaxInt64), CachedInputTokens: int64(cached & math.MaxInt64),
		CacheWriteInputTokens: int64(written & math.MaxInt64), OutputTokens: int64(output & math.MaxInt64),
		ReasoningTokens: int64(reasoning & math.MaxInt64),
		Raw: logJSON(t, struct {
			Tokens  uint64
			Cost    jsontext.Value
			Details []any
		}{input, jsontext.Value(`0.000000000123456789`), []any{text, nil, true, jsontext.Value(`{"nested":9007199254740993}`)}}),
	}
}

func fuzzLogResponse(t *testing.T, text string, usage llm.Usage, index int, variant byte) llm.Response {
	t.Helper()
	usage.Raw = usage.Raw.Clone()
	response := llm.Response{
		ID: fmt.Sprintf("response-%d", index), Usage: usage,
		Stop: []llm.StopReason{llm.StopComplete, llm.StopMaxOutputTokens, llm.StopRefused}[variant%3],
		Output: []llm.Item{
			{ProviderID: fmt.Sprintf("reasoning-%d", index), Type: llm.ItemReasoning, Data: llm.Reasoning{
				Summary: []string{text}, Raw: logJSON(t, struct {
					Encrypted string
					Index     int
				}{text, index}),
			}},
			{ProviderID: fmt.Sprintf("message-%d", index), Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text, Phase: "final_answer"}},
		},
	}
	if variant&1 != 0 {
		response.Failure = &llm.Failure{Code: "provider_failure", Message: text}
	}
	if variant&2 != 0 {
		response.Usage.Raw = nil
	}
	if variant&4 != 0 {
		response.Output = nil
	}
	return response
}

// Keep the oracle's slices independent of values whose ownership is transferred
// to the coordinator. No production JSON codec constructs the oracle.
func copyLogResponse(value llm.Response) llm.Response {
	value.Usage.Raw = value.Usage.Raw.Clone()
	value.Output = slices.Clone(value.Output)
	if value.Failure != nil {
		value.Failure = new(*value.Failure)
	}
	for index, item := range value.Output {
		if reasoning, ok := item.Data.(llm.Reasoning); ok {
			reasoning.Summary = slices.Clone(reasoning.Summary)
			reasoning.Raw = reasoning.Raw.Clone()
			value.Output[index].Data = reasoning
		}
	}
	return value
}

func logJSON(t *testing.T, value any) jsontext.Value {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeLogItems(t *testing.T, encoded []byte) []sessionstore.Item {
	t.Helper()
	if len(encoded) == 0 {
		return nil
	}
	if encoded[len(encoded)-1] != '\n' {
		t.Fatal("log has an unterminated final record")
	}
	var items []sessionstore.Item
	for index, line := range bytes.Split(encoded[:len(encoded)-1], []byte{'\n'}) {
		var item sessionstore.Item
		if err := json.Unmarshal(line, &item); err != nil {
			t.Fatalf("decode JSONL line %d: %v", index+1, err)
		}
		items = append(items, item)
	}
	return items
}

func readLogHistory(t *testing.T, store *localfile.Store, id session.ID, limit int) []sessionstore.Item {
	t.Helper()
	var items []sessionstore.Item
	for after := sessionstore.BeforeFirst; ; {
		page, err := store.Items(t.Context(), id, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if !page.More {
			return items
		}
		if page.NextAfter <= after {
			t.Fatal("history pagination did not advance")
		}
		after = page.NextAfter
	}
}
