package sessionruntime

import (
	"context"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

// BeginClose seals the admission boundary before returning. Existing runtimes
// remain independent owners of their derived keys and resource leases.
func (factory *ReceiverFactory) BeginClose() {
	if factory == nil {
		return
	}
	factory.mu.Lock()
	if factory.closing {
		factory.mu.Unlock()
		return
	}
	factory.closing = true
	factory.cancelAdmissions()
	factory.mu.Unlock()
	go factory.finishClose()
}

func (factory *ReceiverFactory) finishClose() {
	factory.admissions.Wait()
	factory.mu.Lock()
	clear(factory.authKey)
	clear(factory.publicKey)
	factory.authKey = nil
	factory.publicKey = nil
	factory.descriptor = catalog.ShareDescriptor{}
	factory.verifier = nil
	factory.opener = nil
	factory.processReassembly = nil
	factory.shareReassembly = nil
	factory.plaintextProcess = nil
	factory.random = nil
	factory.admissionContext = nil
	factory.cancelAdmissions = nil
	factory.instances = nil
	factory.catalogProgress = nil
	factory.semantic = nil
	factory.resources = nil
	factory.now = nil
	factory.after = nil
	factory.mu.Unlock()
	close(factory.closeDone)
}

func (factory *ReceiverFactory) WaitClosed() {
	if factory != nil {
		<-factory.closeDone
	}
}

func (factory *ReceiverFactory) Close() {
	if factory == nil {
		return
	}
	factory.BeginClose()
	factory.WaitClosed()
}

func (factory *ReceiverFactory) beginAdmission(
	caller context.Context,
) (context.Context, func(), bool) {
	if caller == nil {
		return nil, nil, false
	}
	factory.mu.Lock()
	if factory.closing {
		factory.mu.Unlock()
		return nil, nil, false
	}
	// Add shares the close transition lock so Wait cannot race a late handshake.
	factory.admissions.Add(1)
	lifecycle := factory.admissionContext
	factory.mu.Unlock()
	ctx, cancel := context.WithCancel(caller)
	stopLifecycle := context.AfterFunc(lifecycle, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stopLifecycle()
			cancel()
			factory.admissions.Done()
		})
	}, true
}
