package tool

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
)

func ErrorStatus(message string, limit int) CallStatus {
	if limit <= 0 {
		limit = operation.DefaultMaxOutputLength
	}
	message, truncated := operation.BoundOutput(message, limit)
	return CallStatus{Error: message, ErrorTruncated: truncated}
}

func ParseMaxOutputLength(encoded jsontext.Value) (int, error) {
	if len(encoded) == 0 {
		return operation.DefaultMaxOutputLength, nil
	}
	var limit int
	if err := json.Unmarshal(encoded, &limit); err != nil {
		return 0, fmt.Errorf("decode max_output_length: %w", err)
	}
	if limit <= 0 {
		return 0, errors.New("max_output_length must be a positive integer")
	}
	if limit > operation.MaxOutputLength {
		return 0, fmt.Errorf("max_output_length must not exceed %d", operation.MaxOutputLength)
	}
	return limit, nil
}
