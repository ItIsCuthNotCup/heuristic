package tool

import (
	"reflect"
	"testing"
	"uuid"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
)

type fixedTranslator struct {
	status CallStatus
	result llm.ToolResult
}

type recordingContext struct {
	specs []operation.Spec
}

func (ctx *recordingContext) Submit(spec operation.Spec) operation.ID {
	ctx.specs = append(ctx.specs, spec)
	return "operation-1"
}

func (translator *fixedTranslator) Translate(Context, llm.ToolCall) CallStatus {
	return translator.status
}

func (translator *fixedTranslator) TranslateResult(
	_ string,
	_ CallStatus,
	_ []operation.Operation,
) (llm.ToolResult, error) {
	return translator.result, nil
}

func TestRegistryHasFixedDefinitionsAndInjectedTranslators(t *testing.T) {
	translators := StaticTranslators{
		Bash:      &fixedTranslator{},
		ViewImage: &fixedTranslator{},
	}
	registry := NewRegistry(translators, BashName, ViewImageName, SkillUseName)
	definitions := registry.StaticDefinitions()
	wantNames := []string{BashName, ViewImageName, SkillUseName}
	if len(definitions) != len(wantNames) {
		t.Fatalf("static definitions = %#v", definitions)
	}
	for index, want := range wantNames {
		if definitions[index].Tool.Name != want {
			t.Fatalf("static definition %d name = %q, want %q", index, definitions[index].Tool.Name, want)
		}
	}
	for name, want := range map[string]Translator{BashName: translators.Bash, ViewImageName: translators.ViewImage} {
		got, exists := registry.Resolve(name)
		if !exists || got != want {
			t.Fatalf("resolve %q = (%#v, %t), want (%#v, true)", name, got, exists, want)
		}
	}
	skillUse, exists := registry.Resolve(SkillUseName)
	if !exists {
		t.Fatal("SkillUse is not registered")
	}
	if _, owned := skillUse.(*skillUseTranslator); !owned {
		t.Fatalf("SkillUse translator = %T, want registry-owned translator", skillUse)
	}
	if translator, exists := registry.Resolve("unknown"); exists || translator != nil {
		t.Fatalf("resolve unknown = (%#v, %t), want (nil, false)", translator, exists)
	}
}

func TestRegistryOwnsCanonicalBashDefinition(t *testing.T) {
	got := NewRegistry(StaticTranslators{}, BashName).StaticDefinitions()[0].Tool
	want := llm.Tool{
		Type:        llm.ToolFunction,
		Name:        BashName,
		Description: "Execute a shell command in background. Independent commands may be issued as parallel tool calls in one turn. Command child processes are killed when the shell exits.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute.",
				},
				"max_output_length": map[string]any{"type": "integer", "description": "Maximum characters per output text field. Truncated text keeps its head and tail, around a marker stating how much was omitted, and path to the file with the complete stream. Defaults to 40000.", "minimum": 1, "maximum": 1000000, "default": 40000},
			},
			"required": []any{"command"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bash definition = %#v, want %#v", got, want)
	}
}

func TestRegistryOwnsCanonicalSkillUseDefinition(t *testing.T) {
	got := NewRegistry(StaticTranslators{}, SkillUseName).StaticDefinitions()[0].Tool
	want := llm.Tool{
		Type:        llm.ToolFunction,
		Name:        SkillUseName,
		Description: "Load the instructions for a registered skill.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The exact name of the skill to load.",
				},
			},
			"required": []any{"name"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillUse definition = %#v, want %#v", got, want)
	}
}

func TestRegistryStaticDefinitionsReturnsIndependentValues(t *testing.T) {
	registry := NewRegistry(StaticTranslators{}, BashName)
	definitions := registry.StaticDefinitions()
	definitions[0].Tool.Name = "changed"
	definitions[0].Tool.Parameters["changed"] = true
	got := registry.StaticDefinitions()[0].Tool
	if got.Name != BashName {
		t.Fatalf("static definition name = %q, want %q", got.Name, BashName)
	}
	if _, exists := got.Parameters["changed"]; exists {
		t.Fatal("static definition retained a caller mutation")
	}
}

func TestRegistryRegistersListsAndUnregistersSkills(t *testing.T) {
	registry := NewRegistry(StaticTranslators{})
	first := Skill{Name: "go-review", Description: "Review Go code", Path: "/skills/go/SKILL.md"}
	second := Skill{Name: "documents", Description: "Edit documents", Path: "/skills/docs/SKILL.md"}
	firstID, err := registry.RegisterSkill(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := registry.RegisterSkill(second)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Skills(); !reflect.DeepEqual(got, []Skill{first, second}) {
		t.Fatalf("skills = %#v", got)
	}

	copyOfSkills := registry.Skills()
	copyOfSkills[0].Path = "changed"
	if got := registry.Skills()[0]; got != first {
		t.Fatalf("registered skill = %#v, want %#v", got, first)
	}

	registry.UnregisterSkill(firstID)
	registry.UnregisterSkill(firstID)
	registry.UnregisterSkill(uuid.Nil())
	if got := registry.Skills(); !reflect.DeepEqual(got, []Skill{second}) {
		t.Fatalf("skills after unregister = %#v", got)
	}
	if firstID == secondID {
		t.Fatal("registration IDs are equal")
	}
}

func TestRegistryRejectsInvalidAndDuplicateSkills(t *testing.T) {
	registry := NewRegistry(StaticTranslators{})
	if _, err := registry.RegisterSkill(Skill{}); err == nil || err.Error() != "skill path must be set" {
		t.Fatalf("missing path error = %v", err)
	}
	if _, err := registry.RegisterSkill(Skill{Path: "/skill"}); err == nil ||
		err.Error() != "skill name must be set" {
		t.Fatalf("missing name error = %v", err)
	}
	if _, err := registry.RegisterSkill(Skill{Name: "review", Path: "/skill"}); err == nil ||
		err.Error() != "skill description must be set" {
		t.Fatalf("missing description error = %v", err)
	}
	skill := Skill{Name: "review", Description: "Review code", Path: "/skill"}
	if _, err := registry.RegisterSkill(skill); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RegisterSkill(skill); err == nil ||
		err.Error() != `skill path "/skill" is already registered` {
		t.Fatalf("duplicate path error = %v", err)
	}
	if _, err := registry.RegisterSkill(Skill{
		Name: "review", Description: "Review other code", Path: "/other-skill",
	}); err == nil || err.Error() != `skill name "review" is already registered` {
		t.Fatalf("duplicate name error = %v", err)
	}
}
