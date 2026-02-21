package auth

import "time"

type CooldownConfig struct {
	Initial    time.Duration
	Max        time.Duration
	Multiplier int
}

func DefaultCooldownConfig() CooldownConfig {
	return CooldownConfig{
		Initial:    time.Minute,
		Max:        time.Hour,
		Multiplier: 5,
	}
}

type CooldownTracker struct {
	config CooldownConfig
}

func NewCooldownTracker(cfg CooldownConfig) *CooldownTracker {
	return &CooldownTracker{config: cfg}
}

func (ct *CooldownTracker) PutInCooldown(p *Profile, now time.Time) {
	p.Stats.ErrorCount++
	duration := ct.calculateDuration(p.Stats.ErrorCount)
	p.Stats.CooldownUntil = now.Add(duration)
}

func (ct *CooldownTracker) Disable(p *Profile, now time.Time, duration time.Duration) {
	p.Stats.DisabledUntil = now.Add(duration)
}

func (ct *CooldownTracker) Reset(p *Profile) {
	p.Stats.ErrorCount = 0
	p.Stats.CooldownUntil = time.Time{}
	p.Stats.DisabledUntil = time.Time{}
}

func (ct *CooldownTracker) calculateDuration(errorCount int) time.Duration {
	d := ct.config.Initial
	for i := 1; i < errorCount; i++ {
		d *= time.Duration(ct.config.Multiplier)
		if d > ct.config.Max {
			return ct.config.Max
		}
	}
	return d
}
