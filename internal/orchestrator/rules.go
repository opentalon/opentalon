package orchestrator

import (
	"strings"

	"github.com/opentalon/opentalon/internal/prompts"
)

type RulesConfig struct {
	rules []string
}

func NewRulesConfig(customRules []string) *RulesConfig {
	rules := make([]string, 0, len(prompts.DefaultRules)+len(prompts.SchedulingRules)+len(customRules))
	rules = append(rules, prompts.DefaultRules...)
	rules = append(rules, prompts.SchedulingRules...)

	for _, r := range customRules {
		r = strings.TrimSpace(r)
		if r != "" {
			rules = append(rules, r)
		}
	}

	return &RulesConfig{rules: rules}
}

func builtinRuleCount() int {
	return len(prompts.DefaultRules) + len(prompts.SchedulingRules)
}

func DefaultRulesConfig() *RulesConfig {
	return NewRulesConfig(nil)
}

func (rc *RulesConfig) Rules() []string {
	return rc.rules
}

func (rc *RulesConfig) BuildPromptSection() string {
	section, _ := rc.promptSection()
	return section
}

// promptSection returns the rules section and the size of the lines of the
// operator's rules in it.
func (rc *RulesConfig) promptSection() (section string, operatorBytes int) {
	var sb strings.Builder
	sb.WriteString("## MANDATORY SAFETY RULES\n")
	sb.WriteString("You MUST follow ALL of the following rules at all times. Violation is not permitted under any circumstances.\n\n")

	for i, rule := range rc.rules {
		from := sb.Len()
		if i < builtinRuleCount() {
			sb.WriteString("- ")
		} else {
			sb.WriteString("- [custom] ")
		}
		sb.WriteString(rule)
		sb.WriteString("\n")
		if i >= builtinRuleCount() {
			operatorBytes += sb.Len() - from
		}
	}
	sb.WriteString("\n")
	return sb.String(), operatorBytes
}
