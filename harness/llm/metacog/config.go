package metacog

import "fmt"

// Mode selects which LLM responses the metacognition loop inspects.
type Mode int

const (
	// ModeOff passes every response through untouched.
	ModeOff Mode = iota
	// ModeFinal only judges answers: responses with no tool calls and at
	// least one non-empty assistant message (the default).
	ModeFinal
	// ModeAll judges every response, including tool-call turns.
	ModeAll
)

func ParseMode(value string) (Mode, error) {
	switch value {
	case "off":
		return ModeOff, nil
	case "final", "":
		return ModeFinal, nil
	case "all":
		return ModeAll, nil
	}
	return ModeOff, fmt.Errorf("metacog mode must be off, final or all, got %q", value)
}

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeAll:
		return "all"
	default:
		return "final"
	}
}
