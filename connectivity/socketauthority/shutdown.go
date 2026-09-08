package socketauthority

import "errors"

// A completion retains the result for concurrent close callers. Admission is
// stopped before any socket I/O or worker join starts.
type socketClosure struct {
	done chan struct{}
	err  error
}

func (c *socketClosure) wait() error {
	<-c.done
	return c.err
}

// startCloseLocked keeps closing paths in the capacity ledger until physical
// sockets and their idle workers have stopped. Acquire can wait on just this path.
func (a *Authority) startCloseLocked(entry *pathSockets) *socketClosure {
	if entry.closing != nil {
		return entry.closing
	}
	closing := &socketClosure{done: make(chan struct{})}
	entry.closing = closing
	idle := entry.idle
	if idle != nil {
		idle.cancel()
	}
	go func() {
		err := entry.close()
		if idle != nil {
			<-idle.done
		}
		a.mu.Lock()
		delete(a.paths, entry.key)
		a.socketCount -= entry.socketCount()
		closing.err = err
		close(closing.done)
		a.mu.Unlock()
	}()
	return closing
}

func (a *Authority) Close() error {
	a.mu.Lock()
	if a.closing != nil {
		closing := a.closing
		a.mu.Unlock()
		return closing.wait()
	}
	closing := &socketClosure{done: make(chan struct{})}
	a.closing = closing
	paths := make([]*socketClosure, 0, len(a.paths))
	for _, entry := range a.paths {
		paths = append(paths, a.startCloseLocked(entry))
	}
	a.mu.Unlock()
	var err error
	for _, path := range paths {
		err = errors.Join(err, path.wait())
	}
	closing.err = err
	close(closing.done)
	return err
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	a := l.authority
	a.mu.Lock()
	if !l.released {
		l.released = true
		l.entry.refs--
		if l.entry.refs == 0 {
			a.startCloseLocked(l.entry)
		}
	}
	closing := l.entry.closing
	a.mu.Unlock()
	if closing != nil {
		return closing.wait()
	}
	return nil
}
