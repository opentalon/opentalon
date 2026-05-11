package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/logger"
)

// runTiming tracks wall-clock durations for the major phases of a Run() call.
// Zero-cost when not used: callers only call methods when debug is active.
type runTiming struct {
	start  time.Time
	phases []timedPhase
	active string
	phaseT time.Time
}

type timedPhase struct {
	name     string
	duration time.Duration
}

func newRunTiming() *runTiming {
	now := time.Now()
	return &runTiming{start: now, phaseT: now}
}

// begin marks the start of a named phase. If a previous phase was active,
// it is automatically ended.
func (t *runTiming) begin(name string) {
	now := time.Now()
	if t.active != "" {
		t.phases = append(t.phases, timedPhase{name: t.active, duration: now.Sub(t.phaseT)})
	}
	t.active = name
	t.phaseT = now
}

// end closes the current phase without starting a new one.
func (t *runTiming) end() {
	if t.active == "" {
		return
	}
	t.phases = append(t.phases, timedPhase{name: t.active, duration: time.Since(t.phaseT)})
	t.active = ""
	t.phaseT = time.Now()
}

// log emits the timing breakdown as an Info-level log line.
func (t *runTiming) log(log *logger.Logger) {
	t.end() // close any active phase
	total := time.Since(t.start)

	var parts []string
	for _, p := range t.phases {
		parts = append(parts, fmt.Sprintf("%s=%s", p.name, p.duration.Round(time.Millisecond)))
	}

	attrs := []any{
		"total", total.Round(time.Millisecond),
		"breakdown", strings.Join(parts, " "),
	}
	log.Info("run timing", attrs...)
}
