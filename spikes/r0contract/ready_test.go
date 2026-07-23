package r0contract

import (
	"errors"
	"fmt"
	"testing"
)

const (
	maxReadyRoots         = 4096
	rootEntriesPerPage    = 256
	boundedDescriptorSize = 16 << 10
)

type observableSource struct {
	descendantCount uint64
	rootOpens       int
	descendantOps   int
}

func (source *observableSource) openRoot(_ int) {
	source.rootOpens++
}

type readyCatalog struct {
	failCommit bool
	committed  bool
	rootPages  int
	events     *[]string
}

func (catalog *readyCatalog) commit(rootCount int) error {
	if catalog.failCommit {
		return errors.New("injected root transaction failure")
	}
	catalog.rootPages = (rootCount + rootEntriesPerPage - 1) / rootEntriesPerPage
	catalog.committed = true
	*catalog.events = append(*catalog.events, "catalog-commit")
	return nil
}

type observableRelay struct {
	registerCalls int
	descriptorLen int
	failACK       bool
	linkVisible   bool
	events        *[]string
}

func (relay *observableRelay) register(descriptor []byte) error {
	relay.registerCalls++
	relay.descriptorLen = len(descriptor)
	*relay.events = append(*relay.events, "relay-register")
	if relay.failACK {
		return errors.New("injected relay registration failure")
	}
	return nil
}

func prepareReady(source *observableSource, rootCount int, catalog *readyCatalog, relay *observableRelay) error {
	for root := range rootCount {
		source.openRoot(root)
	}
	if err := catalog.commit(rootCount); err != nil {
		return err
	}
	// The descriptor carries one synthetic-root ID, so its byte size cannot
	// encode or accidentally scale with any descendant cardinality.
	descriptor := []byte("v2:synthetic-root:0123456789abcdef")
	if err := relay.register(descriptor); err != nil {
		return err
	}
	relay.linkVisible = true
	*relay.events = append(*relay.events, "link-visible")
	return nil
}

func TestReadyWorkAndRegistrationBytesIgnoreDescendants(t *testing.T) {
	for _, descendants := range []uint64{0, 1_000, 10_000, 100_000, 1_000_000} {
		t.Run(fmt.Sprint(descendants), func(t *testing.T) {
			var events []string
			source := &observableSource{descendantCount: descendants}
			catalog := &readyCatalog{events: &events}
			relay := &observableRelay{events: &events}
			if err := prepareReady(source, maxReadyRoots, catalog, relay); err != nil {
				t.Fatal(err)
			}
			if source.rootOpens != maxReadyRoots || source.descendantOps != 0 {
				t.Fatalf("root opens/descendant ops = %d/%d", source.rootOpens, source.descendantOps)
			}
			if catalog.rootPages != maxReadyRoots/rootEntriesPerPage {
				t.Fatalf("root pages = %d", catalog.rootPages)
			}
			if relay.descriptorLen > boundedDescriptorSize {
				t.Fatalf("descriptor length = %d", relay.descriptorLen)
			}
			if !relay.linkVisible || fmt.Sprint(events) != "[catalog-commit relay-register link-visible]" {
				t.Fatalf("event order = %v", events)
			}
		})
	}
}

func TestRelayFailureNeverPublishesReadyLink(t *testing.T) {
	var events []string
	source := &observableSource{}
	catalog := &readyCatalog{events: &events}
	relay := &observableRelay{failACK: true, events: &events}
	if err := prepareReady(source, 1, catalog, relay); err == nil {
		t.Fatal("prepareReady unexpectedly succeeded")
	}
	if relay.linkVisible || fmt.Sprint(events) != "[catalog-commit relay-register]" {
		t.Fatalf("failed relay leaked ready state: visible=%t events=%v", relay.linkVisible, events)
	}
}

func TestRootTransactionFailureMakesRegistrationUncallable(t *testing.T) {
	var events []string
	source := &observableSource{descendantCount: 1_000_000}
	catalog := &readyCatalog{failCommit: true, events: &events}
	relay := &observableRelay{events: &events}
	if err := prepareReady(source, 1, catalog, relay); err == nil {
		t.Fatal("prepareReady unexpectedly succeeded")
	}
	if catalog.committed || relay.registerCalls != 0 || len(events) != 0 {
		t.Fatalf("failure leaked state: catalog=%t register=%d events=%v", catalog.committed, relay.registerCalls, events)
	}
	if source.descendantOps != 0 {
		t.Fatalf("failure path touched %d descendants", source.descendantOps)
	}
}
