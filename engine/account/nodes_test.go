package account

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
)

func TestSignedNodeDiscoveryBindsFullWebSocketEndpointWithoutAppendingTwice(t *testing.T) {
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now        int64
		NodeKey    string `json:"node_key"`
		Descriptor json.RawMessage
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := serviceauthority.PublicKey(fixture.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := accountFixture(t)
	config.Now = func() time.Time { return time.Unix(fixture.Now, 0) }
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/.well-known/windshare" || request.Header.Get("Authorization") != "" {
			t.Fatal("discovery leaked authority or used a transfer endpoint", request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(fixture.Descriptor)))}, nil
	})
	service := testService(t, config)
	descriptor, err := service.nodes.discover(t.Context(), "https://node.example", key)
	if err != nil || !descriptor.Anonymous {
		t.Fatal(descriptor, err)
	}
	if _, err := service.nodes.discover(t.Context(), "https://node.example/another-base", key); err == nil {
		t.Fatal("node moved its signed endpoint")
	}
	key[0] ^= 1
	if _, err := service.nodes.discover(t.Context(), "https://node.example", key); err == nil {
		t.Fatal("directory-pinned node key was ignored")
	}
}

func TestNodeDiscoveryStopsWhenLastShareCloses(t *testing.T) {
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now        int64
		NodeKey    string `json:"node_key"`
		Descriptor json.RawMessage
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := serviceauthority.PublicKey(fixture.NodeKey)
	if err != nil {
		t.Fatal(err)
	}

	synctest.Test(t, func(t *testing.T) {
		config, initial := accountFixture(t)
		start := time.Now()
		base := time.Unix(fixture.Now, 0)
		config.Now = func() time.Time { return base.Add(time.Since(start)) }
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		config.ResolveNode = func(address string) (Node, bool) {
			if address == "https://node.example" {
				return Node{Key: key}, true
			}
			return Node{}, false
		}
		var discoveries atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/windshare" {
				discoveries.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(fixture.Descriptor)))}, nil
			}
			return accountReply(t, http.StatusOK, initial), nil
		})
		service := testService(t, config)
		first := service.NewShareAuthorization(t.Context())
		second := service.NewShareAuthorization(t.Context())
		first.Source("https://node.example")
		first.Source("https://node.example")
		second.Source("https://node.example")
		synctest.Wait()
		if got := discoveries.Load(); got != 1 {
			t.Fatalf("initial discoveries=%d, want 1", got)
		}

		first.Close()
		time.Sleep(RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("discoveries with second share active=%d, want 2", got)
		}

		second.Close()
		time.Sleep(3 * RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("node discovery continued after last share closed: %d requests", got)
		}
		if remainingNodes := service.nodes.len(); remainingNodes != 0 {
			t.Fatalf("node map retained %d closed entries", remainingNodes)
		}
	})
}

func TestFileStoreLoadIsLockFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	store := FileStore{Path: path}
	_, initial := accountFixture(t)
	saved := cachedRecord(initial)
	if _, err := store.Update(t.Context(), func(Record) (Record, error) { return saved, nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(t.Context())
	if err != nil || loaded.Ticket != saved.Ticket {
		t.Fatalf("load=%+v err=%v", loaded, err)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock-free load created lock file: err=%v", err)
	}

	// Load must not block even while another writer holds the exclusive lock.
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithLock(t.Context(), path, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	loaded, err = store.Load(t.Context())
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatal(lockErr)
	}
	if err != nil || loaded.Ticket != saved.Ticket {
		t.Fatalf("load under active write lock=%+v err=%v", loaded, err)
	}
}
