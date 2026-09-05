package stunonly

import "time"

type rateLimiter struct {
	config  Config
	window  time.Time
	count   int
	sources map[string]int
}

func newRateLimiter(config Config) *rateLimiter {
	return &rateLimiter{config: config, sources: make(map[string]int)}
}
func (l *rateLimiter) allow(source string, now time.Time) bool {
	if l.window.IsZero() || now.Sub(l.window) >= sourceWindow || now.Before(l.window) {
		l.window = now
		l.count = 0
		clear(l.sources)
	}
	if l.count >= l.config.RequestsPerSecond {
		return false
	}
	count, known := l.sources[source]
	if (!known && len(l.sources) >= l.config.MaximumSources) || count >= l.config.SourceRequestsPerSecond {
		return false
	}
	l.count++
	l.sources[source] = count + 1
	return true
}
