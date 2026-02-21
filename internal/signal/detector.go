package signal

import (
	"strings"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/router"
)

var rejectionPatterns = []string{
	"try again",
	"that's wrong",
	"that is wrong",
	"not what i",
	"no, ",
	"no. ",
	"incorrect",
	"not correct",
	"please redo",
	"redo this",
	"do it again",
	"wrong answer",
	"bad answer",
	"not helpful",
	"useless",
	"doesn't work",
	"does not work",
	"didn't work",
	"did not work",
	"can you fix",
	"please fix",
	"that's not",
	"that is not",
}

type Detector struct{}

func NewDetector() *Detector {
	return &Detector{}
}

func (d *Detector) DetectExplicit(action string) router.Signal {
	switch action {
	case "regenerate":
		return router.SignalRegenerated
	case "thumbs_down":
		return router.SignalRejected
	case "thumbs_up":
		return router.SignalAccepted
	default:
		return router.SignalAccepted
	}
}

func (d *Detector) DetectImplicit(previous *provider.CompletionResponse, followUp *provider.Message) router.Signal {
	if followUp == nil || previous == nil {
		return router.SignalAccepted
	}

	lower := strings.ToLower(followUp.Content)

	for _, pattern := range rejectionPatterns {
		if strings.Contains(lower, pattern) {
			return router.SignalRejected
		}
	}

	if isRephrasedReask(previous.Content, followUp.Content) {
		return router.SignalRejected
	}

	return router.SignalAccepted
}

func isRephrasedReask(previousResponse, followUp string) bool {
	if len(followUp) < 10 || len(previousResponse) < 10 {
		return false
	}

	followUpLower := strings.ToLower(followUp)
	if strings.HasPrefix(followUpLower, "?") || strings.HasSuffix(followUpLower, "?") {
		prevWords := significantWords(previousResponse)
		followWords := significantWords(followUp)
		overlap := wordOverlap(prevWords, followWords)
		return overlap > 0.3
	}
	return false
}

func significantWords(s string) map[string]bool {
	words := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(s)) {
		if len(w) > 3 {
			words[w] = true
		}
	}
	return words
}

func wordOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	count := 0
	smaller, larger := a, b
	if len(a) > len(b) {
		smaller, larger = b, a
	}
	for w := range smaller {
		if larger[w] {
			count++
		}
	}
	return float64(count) / float64(len(smaller))
}
