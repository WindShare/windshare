package catalogflow

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

type DirectorySourceFunc func(context.Context, catalog.DirectoryID) (DirectoryResult, error)

func (f DirectorySourceFunc) LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (DirectoryResult, error) {
	return f(ctx, directory)
}

type ObjectVerifierFunc func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error)

func (f ObjectVerifierFunc) Verify(
	ctx context.Context,
	instance catalog.ShareInstance,
	request ListRequest,
	object []byte,
) (VerifiedObject, error) {
	return f(ctx, instance, request, object)
}

func withFailure(failure DirectoryFailure, mutate func(*DirectoryFailure)) DirectoryFailure {
	mutate(&failure)
	return failure
}

type PageTransportFunc func(context.Context, ListRequest) ([]byte, error)

func (f PageTransportFunc) FetchPage(ctx context.Context, request ListRequest) ([]byte, error) {
	return f(ctx, request)
}

type countingVerifier struct{ calls int }

func (v *countingVerifier) Verify(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
	v.calls++
	return VerifiedObject{}, nil
}

type cancellingTransport struct {
	once      sync.Once
	started   chan struct{}
	cancelled chan struct{}
}

func (t *cancellingTransport) FetchPage(ctx context.Context, _ ListRequest) ([]byte, error) {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	close(t.cancelled)
	return nil, ctx.Err()
}

func waitForWaiters(t *testing.T, client *Client, directory catalog.DirectoryID, count int) {
	t.Helper()
	for {
		client.mu.Lock()
		call := client.inflight[directory]
		ready := call != nil && call.waiters == count
		client.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

type recordingSource struct {
	mu      sync.Mutex
	results map[catalog.DirectoryID]DirectoryResult
	calls   map[catalog.DirectoryID]int
}

func (s *recordingSource) LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (DirectoryResult, error) {
	if err := ctx.Err(); err != nil {
		return DirectoryResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = make(map[catalog.DirectoryID]int)
	}
	s.calls[directory]++
	result, ok := s.results[directory]
	if !ok {
		return DirectoryResult{}, fmt.Errorf("unexpected directory %x", directory)
	}
	return result, nil
}

func (s *recordingSource) CallCount(directory catalog.DirectoryID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[directory]
}

type memoryObjectCodec struct {
	mu      sync.Mutex
	objects map[string]VerifiedObject
}

func newMemoryObjectCodec() *memoryObjectCodec {
	return &memoryObjectCodec{objects: make(map[string]VerifiedObject)}
}

func (c *memoryObjectCodec) LoadSealedPage(_ context.Context, page catalog.CatalogPage) ([]byte, error) {
	key := append([]byte{1}, page.Commitment().Bytes()...)
	c.mu.Lock()
	c.objects[string(key)] = VerifiedPage(page)
	c.mu.Unlock()
	return key, nil
}

func (c *memoryObjectCodec) LoadSealedFailure(_ context.Context, failure DirectoryFailure) ([]byte, error) {
	key := append([]byte{2}, failure.AttemptID.Bytes()...)
	c.mu.Lock()
	c.objects[string(key)] = VerifiedFailure(failure)
	c.mu.Unlock()
	return key, nil
}

func (c *memoryObjectCodec) Verify(
	_ context.Context,
	_ catalog.ShareInstance,
	_ ListRequest,
	encoded []byte,
) (VerifiedObject, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	object, ok := c.objects[string(encoded)]
	if !ok {
		return VerifiedObject{}, errors.New("object authentication failed")
	}
	return object, nil
}

type serviceTransport struct {
	service       *SenderService
	beforeSecond  <-chan struct{}
	secondReached chan struct{}
	once          sync.Once
	mu            sync.Mutex
	calls         int
}

func (t *serviceTransport) FetchPage(ctx context.Context, request ListRequest) ([]byte, error) {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if call == 2 && t.beforeSecond != nil {
		t.once.Do(func() { close(t.secondReached) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.beforeSecond:
		}
	}
	return t.service.Serve(ctx, request)
}

func (t *serviceTransport) CallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

type testCommitter struct{}

func (testCommitter) Commit(input catalog.PageCommitInput) (catalog.PageCommitment, error) {
	hash := sha256.New()
	_, _ = hash.Write(input.ShareInstance.Bytes())
	_, _ = hash.Write(input.DirectoryID.Bytes())
	_, _ = hash.Write(input.Generation.Bytes())
	var index [4]byte
	binary.BigEndian.PutUint32(index[:], input.PageIndex)
	_, _ = hash.Write(index[:])
	_, _ = hash.Write(input.Previous.Bytes())
	for _, entry := range input.Entries {
		_, _ = hash.Write(entry.NodeID().Bytes())
		_, _ = hash.Write([]byte(entry.Name()))
	}
	if input.Terminal {
		_, _ = hash.Write([]byte{1})
	}
	return catalog.NewPageCommitment(hash.Sum(nil))
}

func twoPageSnapshot(t *testing.T, instance catalog.ShareInstance, directory catalog.DirectoryID, generationByte byte, firstName, secondName string) catalog.DirectorySnapshot {
	t.Helper()
	generation := generationID(t, generationByte)
	firstEntry, err := catalog.NewFileEntry(fileID(t, generationByte+1), firstName, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		PageIndex: 0, Entries: []catalog.Entry{firstEntry},
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	secondEntry, err := catalog.NewFileEntry(fileID(t, generationByte+2), secondName, 2, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		PageIndex: 1, Previous: first.Commitment(), Entries: []catalog.Entry{secondEntry}, Terminal: true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{first, second})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func onePageSnapshot(t *testing.T, instance catalog.ShareInstance, directory catalog.DirectoryID, generationByte byte, name string) catalog.DirectorySnapshot {
	t.Helper()
	generation := generationID(t, generationByte)
	entry, err := catalog.NewFileEntry(fileID(t, generationByte+1), name, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		PageIndex: 0, Entries: []catalog.Entry{entry}, Terminal: true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func mustDirectoryFailure(t *testing.T, instance catalog.ShareInstance, directory catalog.DirectoryID, attemptByte byte, code uint16, retryable bool) DirectoryFailure {
	t.Helper()
	failure := DirectoryFailure{
		ShareInstance: instance, DirectoryID: directory, AttemptID: scanAttemptID(t, attemptByte), Code: code,
		Retryable: retryable,
	}
	if retryable {
		failure.RetryAfter = time.Second
	}
	checked, err := NewDirectoryFailure(failure)
	if err != nil {
		t.Fatal(err)
	}
	return checked
}

func fixedIdentity(first byte) []byte {
	result := make([]byte, catalog.IdentityBytes)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func shareInstance(t *testing.T, first byte) catalog.ShareInstance {
	t.Helper()
	value, err := catalog.ShareInstanceFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func directoryID(t *testing.T, first byte) catalog.DirectoryID {
	t.Helper()
	value, err := catalog.DirectoryIDFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fileID(t *testing.T, first byte) catalog.FileID {
	t.Helper()
	value, err := catalog.FileIDFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func generationID(t *testing.T, first byte) catalog.DirectoryGeneration {
	t.Helper()
	value, err := catalog.DirectoryGenerationFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func scanAttemptID(t *testing.T, first byte) catalog.ScanAttemptID {
	t.Helper()
	value, err := catalog.ScanAttemptIDFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
