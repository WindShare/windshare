package connectionlimit

import "testing"

func TestLimiterOwnsTotalAndPerSourceCapacity(t *testing.T) {
	limiter, err := New(Config{MaximumConnections: 2, MaximumConnectionsPerSource: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, allowed := limiter.Admit("one")
	if !allowed {
		t.Fatal("first source was rejected")
	}
	if _, allowed := limiter.Admit("one"); allowed {
		t.Fatal("per-source limit was not enforced")
	}
	second, allowed := limiter.Admit("two")
	if !allowed {
		t.Fatal("second source was rejected")
	}
	if _, allowed := limiter.Admit("three"); allowed {
		t.Fatal("total limit was not enforced")
	}
	first()
	first()
	if snapshot := limiter.Snapshot(); snapshot.Connections != 1 || snapshot.Sources != 1 {
		t.Fatalf("snapshot after idempotent release = %+v", snapshot)
	}
	second()
}

func TestLimiterRejectsInvalidConfiguration(t *testing.T) {
	for _, config := range []Config{
		{},
		{MaximumConnections: 1},
		{MaximumConnections: 1, MaximumConnectionsPerSource: 2},
	} {
		if _, err := New(config); err == nil {
			t.Fatalf("invalid config accepted: %+v", config)
		}
	}
}
