package contextbuilder

import (
	_ "embed"
	"encoding/xml"
	"strings"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

//go:embed prompts/skill-preamble.md
var skillPreambleFile string

var skillPreamble = strings.TrimSpace(skillPreambleFile)

type availableSkills struct {
	XMLName xml.Name      `xml:"available_skills"`
	Skills  []promptSkill `xml:"skill"`
}

type promptSkill struct {
	Name        string `xml:"name"`
	Description string `xml:"description"`
	Location    string `xml:"location"`
}

func formatSkillsForPrompt(skills []tool.Skill) string {
	if len(skills) == 0 {
		return ""
	}

	promptSkills := make([]promptSkill, len(skills))
	for index, skill := range skills {
		promptSkills[index] = promptSkill{
			Name:        skill.Name,
			Description: skill.Description,
			Location:    skill.Path,
		}
	}
	encoded, err := xml.Marshal(availableSkills{Skills: promptSkills})
	if err != nil {
		panic(err)
	}
	return skillPreamble + "\n\n" + string(encoded)
}
