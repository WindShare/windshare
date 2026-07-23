package r0contract

import (
	"bytes"
	"errors"
	"testing"
)

var errFragmentConflict = errors.New("fragment conflicts with authenticated record state")
var errFragmentAdmission = errors.New("fragment authentication or limits rejected admission")

const (
	maxFragmentCount  = 128
	maxReassemblySize = 4*1024*1024 + 512
)

type fragmentAssembly struct {
	count     uint32
	total     int
	fragments map[uint32][]byte
	received  int
	tombstone bool
}

func newFragmentAssembly(count uint32, total int) *fragmentAssembly {
	return &fragmentAssembly{count: count, total: total, fragments: make(map[uint32][]byte, count)}
}

func admitFragmentAssembly(authenticated bool, count uint32, total int) (*fragmentAssembly, error) {
	if !authenticated || count == 0 || count > maxFragmentCount || total < 0 || total > maxReassemblySize {
		return nil, errFragmentAdmission
	}
	return newFragmentAssembly(count, total), nil
}

func (assembly *fragmentAssembly) accept(index uint32, payload []byte, last bool) (bool, error) {
	if assembly.tombstone {
		return false, nil
	}
	if index >= assembly.count || last != (index == assembly.count-1) {
		return false, errFragmentConflict
	}
	if previous, exists := assembly.fragments[index]; exists {
		if !bytes.Equal(previous, payload) {
			return false, errFragmentConflict
		}
		return assembly.complete(), nil
	}
	assembly.fragments[index] = bytes.Clone(payload)
	assembly.received += len(payload)
	if assembly.received > assembly.total {
		return false, errFragmentConflict
	}
	return assembly.complete(), nil
}

func (assembly *fragmentAssembly) complete() bool {
	return len(assembly.fragments) == int(assembly.count) && assembly.received == assembly.total
}

func (assembly *fragmentAssembly) cancel() {
	assembly.fragments = nil
	assembly.received = 0
	assembly.tombstone = true
}

func TestFragmentAssemblerFreezesOutOfOrderDuplicateConflictAndTombstone(t *testing.T) {
	assembly, err := admitFragmentAssembly(true, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if complete, err := assembly.accept(1, []byte("bb"), false); err != nil || complete {
		t.Fatalf("out-of-order middle = %t, %v", complete, err)
	}
	if complete, err := assembly.accept(1, []byte("bb"), false); err != nil || complete {
		t.Fatalf("identical duplicate = %t, %v", complete, err)
	}
	if _, err := assembly.accept(1, []byte("xx"), false); !errors.Is(err, errFragmentConflict) {
		t.Fatalf("conflicting duplicate = %v", err)
	}
	if complete, err := assembly.accept(2, []byte("c"), true); err != nil || complete {
		t.Fatalf("missing first fragment = %t, %v", complete, err)
	}
	if complete, err := assembly.accept(0, []byte("aa"), false); err != nil || !complete {
		t.Fatalf("completed assembly = %t, %v", complete, err)
	}

	assembly.cancel()
	if complete, err := assembly.accept(0, []byte("late"), false); err != nil || complete {
		t.Fatalf("late tombstoned fragment = %t, %v", complete, err)
	}
}

func TestFragmentAssemblerRejectsInvalidLastAndLengthOverflow(t *testing.T) {
	if _, err := newFragmentAssembly(2, 2).accept(0, []byte("a"), true); !errors.Is(err, errFragmentConflict) {
		t.Fatalf("early last = %v", err)
	}
	if _, err := newFragmentAssembly(1, 1).accept(0, []byte("too long"), true); !errors.Is(err, errFragmentConflict) {
		t.Fatalf("length overflow = %v", err)
	}
}

func TestFragmentAssemblerAllocatesOnlyAfterAuthenticationAndLimits(t *testing.T) {
	for _, test := range []struct {
		name          string
		authenticated bool
		count         uint32
		total         int
	}{
		{name: "unauthenticated", count: 1, total: 1},
		{name: "zero count", authenticated: true, total: 1},
		{name: "count over limit", authenticated: true, count: maxFragmentCount + 1, total: 1},
		{name: "bytes over limit", authenticated: true, count: 1, total: maxReassemblySize + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			assembly, err := admitFragmentAssembly(test.authenticated, test.count, test.total)
			if assembly != nil || !errors.Is(err, errFragmentAdmission) {
				t.Fatalf("admission = %#v, %v", assembly, err)
			}
		})
	}
}
