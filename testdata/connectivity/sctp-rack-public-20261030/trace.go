package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
)

type recorder struct {
	mu     sync.Mutex
	start  time.Time
	output io.Writer
	seq    int
	t3     int
	rack   []string
}

func (r *recorder) event(kind string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	fields["event"], fields["seq"] = kind, r.seq
	fields["ms"] = float64(time.Since(r.start).Microseconds()) / 1000
	if err := json.NewEncoder(r.output).Encode(fields); err != nil {
		panic(err)
	}
}
func (r *recorder) factory() *logging.DefaultLoggerFactory {
	return &logging.DefaultLoggerFactory{Writer: r, DefaultLogLevel: logging.LogLevelTrace, ScopeLevels: map[string]logging.LogLevel{}}
}
func (r *recorder) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	r.mu.Lock()
	if strings.Contains(msg, "[sender] T3-rtx timed out:") {
		r.t3++
	}
	if strings.Contains(msg, "[sender] RACK") && strings.Contains(msg, "lost tsn=") {
		r.rack = append(r.rack, msg)
	}
	r.mu.Unlock()
	// Keep recovery evidence without turning a small transfer into a logging benchmark.
	for _, marker := range []string{"RACK", "PTO", "T3-rtx", "measured-rtt=", "retransmit:", "ERROR", "WARNING"} {
		if strings.Contains(msg, marker) {
			r.event("pion", map[string]any{"message": msg})
			break
		}
	}
	return len(p), nil
}
func (r *recorder) counters(target uint32) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	marks := 0
	needle := fmt.Sprintf("tsn=%d", target)
	for _, msg := range r.rack {
		if strings.Contains(msg, needle) {
			marks++
		}
	}
	return r.t3, marks
}
