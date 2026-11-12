// Package logstore keeps bounded asynchronous host evidence off transfer goroutines.
package logstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	queueCapacity      = 1024
	maximumRecordBytes = 128 * 1024
	maximumFileBytes   = 4 * 1024 * 1024
	retainedFiles      = 8
)

type Recorder struct {
	mu        sync.Mutex
	queue     chan []byte
	done      chan struct{}
	closed    bool
	dropped   atomic.Uint64
	directory string
	session   string
	file      *os.File
	size      int
	sequence  uint64
	err       error
}

// Disabled retains the startup failure without making diagnostics an admission
// dependency. No queue or writer is started when storage cannot be opened.
func Disabled(session string, err error) *Recorder {
	done := make(chan struct{})
	close(done)
	return &Recorder{session: session, closed: true, done: done, err: err}
}

func Open(directory, session string) (*Recorder, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	r := &Recorder{queue: make(chan []byte, queueCapacity), done: make(chan struct{}), directory: directory, session: session}
	if err := r.rotate(); err != nil {
		return nil, err
	}
	go r.write()
	return r, nil
}

func (r *Recorder) Record(event string, fields any) {
	data, err := json.Marshal(map[string]any{"at": time.Now().UTC(), "session_id": r.session, "event": event, "fields": fields})
	if err != nil || len(data) > maximumRecordBytes {
		r.dropped.Add(1)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- append(data, '\n'):
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) write() {
	defer close(r.done)
	for record := range r.queue {
		if r.err != nil {
			r.dropped.Add(1)
			continue
		}
		if r.size+len(record) > maximumFileBytes {
			r.err = errors.Join(r.file.Sync(), r.file.Close())
			r.file = nil
			if r.err == nil {
				r.err = r.rotate()
			}
		}
		if r.err == nil {
			var n int
			n, r.err = r.file.Write(record)
			r.size += n
		}
	}
	if r.file != nil {
		_, err := fmt.Fprintf(r.file, "{\"event\":\"host.log_closed\",\"dropped\":%d}\n", r.dropped.Load())
		r.err = errors.Join(r.err, err, r.file.Sync(), r.file.Close())
	}
}

func (r *Recorder) rotate() error {
	entries, err := filepath.Glob(filepath.Join(r.directory, "host-*.jsonl"))
	if err != nil {
		return err
	}
	slices.Sort(entries)
	for len(entries) >= retainedFiles {
		// Another running process may own an old Windows file. Its writer enforces
		// the same size bound; a later rotation retries pruning it.
		_ = os.Remove(entries[0])
		entries = entries[1:]
	}
	r.sequence++
	name := fmt.Sprintf("host-%s-%s-%04d.jsonl", time.Now().UTC().Format("20060102T150405.000000000"), r.session, r.sequence)
	r.file, err = os.OpenFile(filepath.Join(r.directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	r.size = 0
	return err
}

func (r *Recorder) Close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
	return r.err
}
