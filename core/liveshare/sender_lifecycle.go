package liveshare

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/sendersession"
)

type senderRuntimeLifecycle interface {
	BeginStop(string) error
	Stop(context.Context, string) error
}

type senderOwnedResources struct {
	runtimeFactory senderRuntimeLifecycle
	cache          *contentflow.SharedBlockCache
	catalogAccess  *senderCatalogAccess
	revisionStore  *content.RevisionStore
	catalogStore   *catalog.CatalogStore
	source         FileSource
	keyTree        *content.KeyTree
}

// Stop synchronously freezes admission, then transfers teardown to one worker.
func (sender *PreparedSender) Stop() error {
	if sender == nil {
		return nil
	}
	sender.mu.Lock()
	if sender.closed {
		sender.mu.Unlock()
		return nil
	}
	sender.closed = true
	done := make(chan struct{})
	sender.closeDone = done
	resources := senderOwnedResources{
		runtimeFactory: sender.runtimeFactory,
		cache:          sender.cache,
		catalogAccess:  sender.catalogAccess,
		revisionStore:  sender.revisionStore,
		catalogStore:   sender.catalogStore,
		source:         sender.source,
		keyTree:        sender.keyTree,
	}
	var beginErr error
	if resources.runtimeFactory != nil {
		// BeginStop performs no callbacks or joins, so holding this lock ensures
		// every concurrent Stop returns only after borrowed factory admission froze.
		beginErr = resources.runtimeFactory.BeginStop("Sender closed")
	}
	sender.mu.Unlock()

	go sender.finishClose(done, resources, beginErr)
	return beginErr
}

// Close is the external ownership boundary. Dependency callbacks call Stop;
// synchronously joining the teardown worker from inside it cannot make progress.
func (sender *PreparedSender) Close() error {
	if sender == nil {
		return nil
	}
	_ = sender.Stop()
	sender.mu.Lock()
	done := sender.closeDone
	sender.mu.Unlock()
	if done != nil {
		<-done
	}
	sender.mu.Lock()
	result := sender.closeResult
	sender.mu.Unlock()
	return result
}

func (sender *PreparedSender) finishClose(
	done chan struct{},
	resources senderOwnedResources,
	beginErr error,
) {
	result := beginErr
	if resources.runtimeFactory != nil {
		// Join the runtime factory before closing shared stores and keys. Remote
		// connectivity cleanup errors belong to the Factory.Stop caller; Close
		// retains only local resource-teardown failures.
		if stopErr := resources.runtimeFactory.Stop(context.Background(), "Sender closed"); errors.Is(stopErr, sendersession.ErrSessionCleanup) {
			result = errors.Join(result, stopErr)
		}
	}
	// Cache loaders can still hold revision-source handles and content keys after
	// their waiters unblock, so dependent authority survives until the join.
	if resources.cache != nil {
		resources.cache.Close()
	}

	if resources.catalogAccess != nil {
		resources.catalogAccess.Close()
	}
	if resources.revisionStore != nil {
		result = errors.Join(result, resources.revisionStore.Close())
	}
	if resources.catalogStore != nil {
		result = errors.Join(result, resources.catalogStore.Close())
	}
	if resources.source != nil {
		result = errors.Join(result, resources.source.Close())
	}
	// Quiescence precedes destruction because both sealers retain cloned key
	// authority that active catalog or content work may still dereference.

	sender.mu.Lock()
	clear(sender.capability.ReadSecret)
	sender.capability.ReadSecret = nil
	clear(sender.sessionAuthKey)
	sender.sessionAuthKey = nil
	clear(sender.privateKey)
	sender.privateKey = nil
	sender.mu.Unlock()

	if resources.keyTree != nil {
		resources.keyTree.Destroy()
	}

	sender.mu.Lock()
	// Completed ownership must not keep destroyed authorities, filesystem roots,
	// or caller-provided entropy reachable through a closed facade.
	sender.runtimeFactory = nil
	sender.cache = nil
	sender.catalogAccess = nil
	sender.revisionStore = nil
	sender.catalogStore = nil
	sender.source = nil
	sender.keyTree = nil
	sender.random = nil
	sender.capability = link.Link{}
	sender.descriptor = catalog.ShareDescriptor{}
	sender.descriptorObject = nil
	sender.committedRoot = catalog.CommittedRoot{}
	sender.shareIDRaw = nil
	sender.pkHash = nil
	sender.closeResult = result
	close(done)
	sender.mu.Unlock()
}
