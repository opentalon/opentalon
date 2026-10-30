package orchestrator

import (
	"context"
	"regexp"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
)

// A turn the person spoke (actor.SpokenReply, set from a channel's inbound
// metadata["reply"] == "spoken") gets its answer read aloud. The model writes a
// short spoken version at the start of its reply to the user, between markers
// (spokenReplyInstruction, added to the per-round system segment in every round
// by appendConversation, so the cached prefix stays the same). Run takes it out of every response the moment it arrives
// (takeSpoken), so tool parsing, the checks on the response, what is stored and
// what is shown never see the markers; the spoken version of the final reply goes
// into the result's metadata and onto the stored reply (spokenMetadata), and a
// final reply without one is stored with spoken_missing and stays silent. The
// confirmation question is written by its own narrator, which in a spoken turn
// writes the question as it is asked aloud the same way
// (spokenConfirmationInstruction). Text no model wrote a spoken version for (a
// confirmation the narrator wrote without one, a plan, a pipeline's summary, any
// other way a turn ends) is its own spoken version when it is short enough
// (speakAsIs; Run applies it to every result that carries neither key). Whatever
// is read goes through speakable first: what a screen shows and an ear cannot
// take (lists, field changes, markdown) is never read. Spoken turns are not
// streamed: the spoken version must be out before anything is shown.

const (
	// SpokenMetadataKey carries the spoken version on the result and the stored reply.
	SpokenMetadataKey = "spoken"
	// SpokenMissingMetadataKey marks a stored reply that should have had a spoken version and had none.
	SpokenMissingMetadataKey = "spoken_missing"
	// InputMetadataKey with InputVoice marks a stored user turn the person spoke.
	InputMetadataKey = "input"
	InputVoice       = "voice"
	// ReplyMetadataKey with ReplySpoken asks for a spoken answer; a channel echoes it on the answer's frame.
	ReplyMetadataKey = "reply"
	ReplySpoken      = "spoken"

	spokenOpen  = "<spoken>"
	spokenClose = "</spoken>"
	// spokenLimit caps what is read aloud, in characters.
	spokenLimit = 400
)

const spokenReplyInstruction = "## SPOKEN REPLY\n" +
	"The user spoke this message and will hear your reply read aloud. When you answer the user (not when you " +
	"call a tool), begin the answer with a spoken version between " + spokenOpen + " and " + spokenClose + ": " +
	"one or two short sentences in the language of your answer, as you would say them aloud, at most 300 " +
	"characters. No lists, tables, markdown, ids, record numbers, links or code in it; say what you found, did or " +
	"need, and that the details are in the chat when there are any. Then write your answer as you otherwise " +
	"would. Write the markers once, at the very start.\n"

const spokenConfirmationInstruction = "## SPOKEN QUESTION\n" +
	"The user spoke their request and will hear this question read aloud. Begin your reply with the question as " +
	"you would ask it aloud, between " + spokenOpen + " and " + spokenClose + ": one or two short sentences in " +
	"the language of your reply, at most 300 characters, under the same rules as the question for the screen. " +
	"Name what will be created, changed or deleted by its name and say a change in words, never as a field list " +
	"or with arrows; no markdown, ids, links or code in it. Then write the question for the screen as the rules " +
	"above say. Write the markers once, at the very start.\n"

// splitSpoken returns the first spoken version a response carries and the
// response without any: every block goes, and a marker the model left unpaired
// or nested goes with it (an unclosed block keeps its text in the response).
// found is false when it carries none.
func splitSpoken(response string) (spoken, rest string, found bool) {
	var kept strings.Builder
	remaining := response
	for {
		start := strings.Index(remaining, spokenOpen)
		if start < 0 {
			kept.WriteString(remaining)
			break
		}
		kept.WriteString(remaining[:start])
		inner := remaining[start+len(spokenOpen):]
		end := strings.Index(inner, spokenClose)
		if end < 0 {
			kept.WriteString(inner)
			break
		}
		if text := strings.TrimSpace(markers.Replace(inner[:end])); !found && text != "" {
			spoken, found = text, true
		}
		remaining = inner[end+len(spokenClose):]
	}
	rest = strings.TrimSpace(markers.Replace(kept.String()))
	return spoken, rest, found
}

var markers = strings.NewReplacer(spokenOpen, "", spokenClose, "")

// takeSpoken takes the spoken version out of a model response in a spoken turn;
// in any other turn the response stays as it is. A response that was nothing but
// its spoken version keeps that, whole, as its text.
func takeSpoken(ctx context.Context, response string) (spoken, rest string) {
	if !actor.SpokenReply(ctx) || (!strings.Contains(response, spokenOpen) && !strings.Contains(response, spokenClose)) {
		return "", response
	}
	spoken, rest, found := splitSpoken(response)
	if found && rest == "" {
		rest = spoken
	}
	return spoken, rest
}

// spokenMetadata is the metadata of a final reply in a spoken turn: its spoken
// version, or spoken_missing when it had none or one too long to read. nil in
// any other turn.
func spokenMetadata(ctx context.Context, spoken string) map[string]string {
	return speakAsIs(ctx, spoken, nil)
}

// markUnspoken gives a spoken turn's result that carries neither key the
// treatment of text written without markers (speakAsIs): every way a turn can
// end — a cancellation, a fallback, a command — tells the channel what to read.
func markUnspoken(ctx context.Context, result *RunResult) {
	if result == nil || !actor.SpokenReply(ctx) {
		return
	}
	if _, ok := result.Metadata[SpokenMetadataKey]; ok {
		return
	}
	if _, ok := result.Metadata[SpokenMissingMetadataKey]; ok {
		return
	}
	result.Metadata = speakAsIs(ctx, result.Response, result.Metadata)
}

// speakAsIs adds to meta, in a spoken turn, what of the text can be read aloud
// (speakable) as its spoken version when it is short enough, or spoken_missing
// when it is not or nothing is left (cut in the middle of a sentence, it would
// say less than silence). meta comes back as is in any other turn.
func speakAsIs(ctx context.Context, text string, meta map[string]string) map[string]string {
	if !actor.SpokenReply(ctx) {
		return meta
	}
	out := make(map[string]string, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	text = speakable(text)
	if text != "" && len([]rune(text)) <= spokenLimit {
		out[SpokenMetadataKey] = text
	} else {
		out[SpokenMissingMetadataKey] = "true"
	}
	return out
}

var (
	speechHeading  = regexp.MustCompile(`^#{1,6}(\s|$)`)
	speechRule     = regexp.MustCompile(`^([-*_]\s*){3,}$`)
	speechBullet   = regexp.MustCompile(`^[-*+•]\s`)
	speechNumbered = regexp.MustCompile(`^\d+[.)]\s`)
	speechImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	speechLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	speechAddress  = regexp.MustCompile(`(https?://|www\.)\S+`)
	speechIndented = regexp.MustCompile(`^( {4}|\t)`)
	speechFence    = regexp.MustCompile("^(`{3,}|~{3,})")
	// Emphasis marks touch a word on their inner side and no letter or digit on
	// their outer one; a lone "*" or "~" between spaces or digits is not one.
	speechOpening = regexp.MustCompile(`(^|[^\p{L}\p{N}*_~])(\*+|_+|~~)(\S)`)
	speechClosing = regexp.MustCompile(`(\S)(\*+|_+|~~)([^\p{L}\p{N}*_~]|$)`)
)

// speakable is what of a text written for a screen can be read aloud: the
// sentences, without their markdown (a link keeps its words, an identifier's
// underscores become spaces). A line that is structure rather than a sentence
// (a heading, a list item, a table row, a field change "field: old -> new",
// code, an indented line, which is code or belongs to a list) is left out whole,
// quoted or not: read aloud it is noise, and the person sees it in the chat. ""
// when nothing is left.
func speakable(text string) string {
	var lines []string
	fence := ""
	for _, raw := range strings.Split(text, "\n") {
		body := unquoted(raw)
		line := strings.TrimSpace(body)
		indented := speechIndented.MatchString(body)
		if mark := speechFence.FindString(line); mark != "" && !indented && (fence == "" || closesFence(line, mark, fence)) {
			if fence == "" {
				fence = mark
			} else {
				fence = ""
			}
			continue
		}
		if fence == "" && line != "" && !indented {
			lines = append(lines, line)
		}
	}
	numbered := 0
	for _, line := range lines {
		if speechNumbered.MatchString(line) {
			numbered++
		}
	}
	var kept []string
	for _, line := range lines {
		if !structural(line, numbered > 1) {
			kept = append(kept, plainWords(line))
		}
	}
	return strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
}

// unquoted is the line without its blockquote marks (each ">" with one space
// after it), its own indentation kept.
func unquoted(line string) string {
	rest := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(rest, ">") || len(line)-len(rest) > 3 {
		return line
	}
	for strings.HasPrefix(rest, ">") {
		rest = strings.TrimPrefix(rest[1:], " ")
	}
	return rest
}

// closesFence tells whether a fence line closes the open code block: the same
// character, at least as long, nothing after it.
func closesFence(line, mark, open string) bool {
	return mark[0] == open[0] && len(mark) >= len(open) && strings.TrimSpace(line[len(mark):]) == ""
}

// structural tells a line that is not a sentence. A numbered line is a list
// item only when the text has another (inList), so "1. Januar ..." stays a sentence.
func structural(line string, inList bool) bool {
	return strings.Contains(line, "|") || strings.Contains(line, "->") || strings.Contains(line, "→") ||
		speechHeading.MatchString(line) || speechRule.MatchString(line) || speechBullet.MatchString(line) ||
		(inList && speechNumbered.MatchString(line))
}

func plainWords(line string) string {
	line = speechImage.ReplaceAllString(line, "")
	line = speechLink.ReplaceAllString(line, "$1")
	line = speechAddress.ReplaceAllString(line, "")
	line = strings.ReplaceAll(line, "`", "")
	line = speechOpening.ReplaceAllString(line, "$1$3")
	line = speechClosing.ReplaceAllString(line, "$1$3")
	return strings.ReplaceAll(line, "_", " ")
}

// voiceInputMetadata marks a user turn the person spoke; extra (a confirmation
// reply's own metadata) is kept. extra comes back as is in any other turn.
func voiceInputMetadata(ctx context.Context, extra map[string]string) map[string]string {
	if !actor.SpokenReply(ctx) {
		return extra
	}
	meta := map[string]string{InputMetadataKey: InputVoice}
	for k, v := range extra {
		meta[k] = v
	}
	return meta
}

// resumedSpoken gives a re-emitted confirmation (PendingConfirmationFrame) the
// reply=spoken its live frame carried as the echo of the spoken message: a
// client answers it aloud, whichever button is pressed.
func resumedSpoken(meta map[string]string, spoken bool) map[string]string {
	if spoken {
		meta[ReplyMetadataKey] = ReplySpoken
	}
	return meta
}

// mergeMetadata adds extra to the result's metadata.
func mergeMetadata(result *RunResult, extra map[string]string) {
	if len(extra) == 0 {
		return
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]string, len(extra))
	}
	for k, v := range extra {
		result.Metadata[k] = v
	}
}
