package task

// Changes returns a broadcast generation. Read it before querying state, then
// wait for it to close; publication between either step cannot be missed.
// Consumers retain no subscription and never block workflow progress.
func (current *Task[T]) Changes() <-chan struct{} {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.changed
}

func (current *Task[T]) notifyChanged() {
	current.mu.Lock()
	defer current.mu.Unlock()
	current.notifyLocked()
}

func (current *Task[T]) notifyLocked() {
	close(current.changed)
	current.changed = make(chan struct{})
}
