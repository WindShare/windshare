package relayv2_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testprocess"
	"github.com/windshare/windshare/internal/testrelaynative"
)

const relayBuildTimeout = 5 * time.Minute

var relaySuite struct {
	sync.Once
	root      string
	directory string
	binary    string
	owner     *testprocess.Owner
	err       error
}

func TestMain(m *testing.M) {
	code := m.Run()
	var ownerErr error
	if relaySuite.owner != nil {
		ownerErr = relaySuite.owner.Close()
	}
	var directoryErr error
	if relaySuite.directory != "" {
		directoryErr = os.RemoveAll(relaySuite.directory)
	}
	if err := errors.Join(ownerErr, directoryErr); err != nil {
		fmt.Fprintln(os.Stderr, "relay integration: clean build fixture:", err)
		code = 1
	}
	os.Exit(code)
}

func prepareRelaySuite(t *testing.T) {
	t.Helper()
	// Keep short tests and filtered behavior tests independent of Rust tooling.
	relaySuite.Do(func() { relaySuite.err = buildRelaySuite() })
	if relaySuite.err != nil {
		t.Fatalf("prepare native relay integration suite: %v", relaySuite.err)
	}
}

func buildRelaySuite() error {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return err
	}
	relaySuite.root = root
	directory, err := os.MkdirTemp("", "windshare-relay-integration-bin-")
	if err != nil {
		return err
	}
	relaySuite.directory = directory
	ctx, cancel := context.WithTimeout(context.Background(), relayBuildTimeout)
	defer cancel()
	relaySuite.binary, err = testrelaynative.Build(ctx, root, directory)
	if err != nil {
		return err
	}
	relaySuite.owner, err = testprocess.BuildOwner(ctx, root)
	return err
}
