package prompts

import (
	_ "embed"
	"strings"
)

//go:embed planner_preamble.txt
var PlannerPreamble string

//go:embed planner_suffix.txt
var plannerSuffixRaw string

// PlannerSuffix has no trailing newline; callers control the preceding separator.
var PlannerSuffix = strings.TrimRight(plannerSuffixRaw, "\n")

//go:embed planner_narrate.txt
var plannerNarrateRaw string

// PlannerNarrate has no trailing newline; callers append language suffix when needed.
var PlannerNarrate = strings.TrimRight(plannerNarrateRaw, "\n")

//go:embed orchestrator_preamble.txt
var OrchestratorPreamble string

//go:embed orchestrator_subprocess.txt
var OrchestratorSubprocess string

//go:embed orchestrator_summarize.txt
var summarizeDefaultRaw string

var SummarizeDefault = strings.TrimRight(summarizeDefaultRaw, "\n")

//go:embed orchestrator_summarize_update.txt
var summarizeUpdateRaw string

var SummarizeUpdate = strings.TrimRight(summarizeUpdateRaw, "\n")

//go:embed subprocess_preamble.txt
var SubprocessPreamble string

//go:embed rules_default.txt
var rulesDefaultRaw string

var DefaultRules = splitLines(rulesDefaultRaw)

//go:embed rules_scheduling.txt
var rulesSchedulingRaw string

var SchedulingRules = splitLines(rulesSchedulingRaw)

//go:embed format_slack.txt
var formatSlackRaw string

var FormatSlack = strings.TrimRight(formatSlackRaw, "\n")

//go:embed format_markdown.txt
var formatMarkdownRaw string

var FormatMarkdown = strings.TrimRight(formatMarkdownRaw, "\n")

//go:embed format_html.txt
var formatHTMLRaw string

var FormatHTML = strings.TrimRight(formatHTMLRaw, "\n")

//go:embed format_telegram.txt
var formatTelegramRaw string

var FormatTelegram = strings.TrimRight(formatTelegramRaw, "\n")

//go:embed format_teams.txt
var formatTeamsRaw string

var FormatTeams = strings.TrimRight(formatTeamsRaw, "\n")

//go:embed format_whatsapp.txt
var formatWhatsAppRaw string

var FormatWhatsApp = strings.TrimRight(formatWhatsAppRaw, "\n")

//go:embed format_discord.txt
var formatDiscordRaw string

var FormatDiscord = strings.TrimRight(formatDiscordRaw, "\n")

//go:embed format_text.txt
var formatTextRaw string

var FormatText = strings.TrimRight(formatTextRaw, "\n")

func splitLines(s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
