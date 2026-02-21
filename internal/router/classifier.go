package router

import (
	"strings"

	"github.com/opentalon/opentalon/internal/provider"
)

var transformKeywords = []string{
	"translate", "summarize", "summarise", "convert", "rewrite",
	"paraphrase", "rephrase", "format", "extract",
}

type TaskClassifier struct {
	deepConversationThreshold int
}

func NewTaskClassifier() *TaskClassifier {
	return &TaskClassifier{
		deepConversationThreshold: 10,
	}
}

func (c *TaskClassifier) Classify(messages []provider.Message) TaskType {
	if len(messages) == 0 {
		return TaskGeneral
	}

	if len(messages) > c.deepConversationThreshold {
		return TaskDeepConversation
	}

	last := messages[len(messages)-1]
	content := last.Content
	lower := strings.ToLower(content)

	if containsCodeBlock(content) {
		return TaskCode
	}

	for _, kw := range transformKeywords {
		if strings.Contains(lower, kw) {
			return TaskTransform
		}
	}

	if len(content) > 500 {
		return TaskAnalysis
	}

	if len(content) < 100 && !strings.Contains(content, "\n") {
		return TaskChat
	}

	return TaskGeneral
}

func containsCodeBlock(s string) bool {
	return strings.Contains(s, "```") ||
		strings.Contains(s, "func ") ||
		strings.Contains(s, "def ") ||
		strings.Contains(s, "class ") ||
		strings.Contains(s, "import ") ||
		strings.Contains(s, "package ")
}
