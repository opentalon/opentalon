package prompts

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// promptFS holds all .txt files for hashing. Individual vars use //go:embed for
// typed access; this FS is the authoritative source for Hash().
//
//go:embed *.txt
var promptFS embed.FS

// Hash returns the sha256 digest of all prompt files, sorted by filename.
// Cassette metadata stores this value; a mismatch means prompts changed since
// recording and the cassette must be re-recorded.
func Hash() string {
	entries, err := promptFS.ReadDir(".")
	if err != nil {
		panic(fmt.Sprintf("prompts: failed to read embedded FS: %v", err))
	}
	h := sha256.New()
	for _, e := range entries {
		data, err := promptFS.ReadFile(e.Name())
		if err != nil {
			panic(fmt.Sprintf("prompts: failed to read %s: %v", e.Name(), err))
		}
		_, _ = fmt.Fprintf(h, "%s\n", e.Name())
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

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

//go:embed planner_narrate_tool.txt
var plannerNarrateToolRaw string

// PlannerNarrateTool has no trailing newline; callers append language suffix when needed.
var PlannerNarrateTool = strings.TrimRight(plannerNarrateToolRaw, "\n")

//go:embed orchestrator_preamble.txt
var OrchestratorPreamble string

//go:embed orchestrator_preamble_native.txt
var OrchestratorPreambleNative string

//go:embed orchestrator_subprocess.txt
var OrchestratorSubprocess string

//go:embed orchestrator_summarize.txt
var summarizeDefaultRaw string

var SummarizeDefault = strings.TrimRight(summarizeDefaultRaw, "\n")

//go:embed orchestrator_summarize_update.txt
var summarizeUpdateRaw string

var SummarizeUpdate = strings.TrimRight(summarizeUpdateRaw, "\n")

//go:embed orchestrator_session_title.txt
var sessionTitleRaw string

// SessionTitle is the system prompt the title-generation pass sends to the
// LLM. Output contract: a short title (3–6 words), no quotes, in the
// user's language. Trailing newline trimmed so callers don't have to.
var SessionTitle = strings.TrimRight(sessionTitleRaw, "\n")

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

// promptSetters maps each built-in prompt's canonical name — its .txt filename
// without the extension — to a function that overrides the corresponding
// exported value, applying the same post-processing the embedded default gets
// (raw, trailing-newline-trimmed, or line-split). ApplyOverrides uses it so
// every built-in prompt is configurable without touching any call site.
var promptSetters = map[string]func(string){
	"orchestrator_preamble":         func(s string) { OrchestratorPreamble = s },
	"orchestrator_preamble_native":  func(s string) { OrchestratorPreambleNative = s },
	"orchestrator_subprocess":       func(s string) { OrchestratorSubprocess = s },
	"subprocess_preamble":           func(s string) { SubprocessPreamble = s },
	"planner_preamble":              func(s string) { PlannerPreamble = s },
	"planner_suffix":                func(s string) { PlannerSuffix = strings.TrimRight(s, "\n") },
	"planner_narrate":               func(s string) { PlannerNarrate = strings.TrimRight(s, "\n") },
	"planner_narrate_tool":          func(s string) { PlannerNarrateTool = strings.TrimRight(s, "\n") },
	"orchestrator_summarize":        func(s string) { SummarizeDefault = strings.TrimRight(s, "\n") },
	"orchestrator_summarize_update": func(s string) { SummarizeUpdate = strings.TrimRight(s, "\n") },
	"orchestrator_session_title":    func(s string) { SessionTitle = strings.TrimRight(s, "\n") },
	"rules_default":                 func(s string) { DefaultRules = splitLines(s) },
	"rules_scheduling":              func(s string) { SchedulingRules = splitLines(s) },
	"format_slack":                  func(s string) { FormatSlack = strings.TrimRight(s, "\n") },
	"format_markdown":               func(s string) { FormatMarkdown = strings.TrimRight(s, "\n") },
	"format_html":                   func(s string) { FormatHTML = strings.TrimRight(s, "\n") },
	"format_telegram":               func(s string) { FormatTelegram = strings.TrimRight(s, "\n") },
	"format_teams":                  func(s string) { FormatTeams = strings.TrimRight(s, "\n") },
	"format_whatsapp":               func(s string) { FormatWhatsApp = strings.TrimRight(s, "\n") },
	"format_discord":                func(s string) { FormatDiscord = strings.TrimRight(s, "\n") },
	"format_text":                   func(s string) { FormatText = strings.TrimRight(s, "\n") },
}

// OverridableNames returns the sorted canonical names ApplyOverrides accepts —
// one per built-in prompt. Useful for config validation and documentation.
func OverridableNames() []string {
	names := make([]string, 0, len(promptSetters))
	for name := range promptSetters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ApplyOverrides replaces built-in prompt defaults with values supplied from
// config, keyed by canonical prompt name (the .txt filename without extension).
// Every key present is applied, including an empty value — that blanks the
// prompt, which is how a deployment removes a built-in block it does not want
// (e.g. set "rules_scheduling" to "" where no scheduler plugin is loaded). To
// keep a default, omit its key. Unknown keys are returned (sorted) so the caller
// can warn about typos; applied keys are returned for logging.
//
// It mutates package-level defaults, so call it once at startup before the
// orchestrator serves traffic — it is not safe to run concurrently with prompt
// reads. It does NOT change how plugin / MCP server instructions get appended to
// the system prompt; the orchestrator adds those separately at request time.
func ApplyOverrides(overrides map[string]string) (applied, unknown []string) {
	for name, text := range overrides {
		setter, ok := promptSetters[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		setter(text)
		applied = append(applied, name)
	}
	sort.Strings(applied)
	sort.Strings(unknown)
	return applied, unknown
}
