package receiversession

import (
	"context"
	"github.com/windshare/windshare/core/catalog"
)

// BeginClose seals the admission boundary before returning. Existing runtimes
// remain independent owners of their derived keys and resource leases.
func (factory *Factory) BeginClose() {
	if factory == nil || !factory.admissions.Seal() {
		return
	}
	go factory.finishClose()
}

func (factory *Factory) finishClose() {
	factory.admissions.Close()
	factory.mu.Lock()
	clear(factory.authKey)
	factory.authKey = nil
	factory.sender = nil
	factory.descriptor = catalog.ShareDescriptor{}
	factory.verifier = nil
	factory.opener = nil
	factory.processReassembly = nil
	factory.shareReassembly = nil
	factory.plaintextProcess = nil
	factory.random = nil
	factory.instances = nil
	factory.catalogProgress = nil
	factory.semantic = nil
	factory.resources = nil
	factory.now = nil
	factory.after = nil
	factory.mu.Unlock()
	close(factory.closeDone)
}

func (factory *Factory) WaitClosed() {
	if factory != nil {
		<-factory.closeDone
	}
}

func (factory *Factory) Close() {
	if factory == nil {
		return
	}
	factory.BeginClose()
	factory.WaitClosed()
}

func (factory *Factory) beginAdmission(
	caller context.Context,
) (context.Context, func(), bool) {
	if factory == nil {
		return nil, nil, false
	}
	return factory.admissions.Begin(caller)
}
