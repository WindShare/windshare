package relayconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/internal/testprocess"
	"github.com/windshare/windshare/internal/testrun"
)

const transactionProbePath = "WINDSHARE_CONFIG_TRANSACTION_PROBE"

func TestTransactionsReadLatestSettingsAcrossIndependentStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (FileStore{Path: path}).Save(Config{Profiles: []senderrelay.Candidate{}}); err != nil {
		t.Fatal(err)
	}
	const writers = 12
	var group sync.WaitGroup
	for index := range writers {
		group.Go(func() {
			_, err := (FileStore{Path: path}).Update(t.Context(), func(current *Config) error {
				runtime.Gosched()
				current.Profiles = append(current.Profiles, senderrelay.Candidate{PublicURL: fmt.Sprintf("https://relay-%d.example", index)})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	saved, err := (FileStore{Path: path}).Load()
	if err != nil || len(saved.Profiles) != writers {
		t.Fatalf("lost a committed modification: %+v, %v", saved, err)
	}
	_, err = (FileStore{Path: path}).Update(t.Context(), func(current *Config) error {
		current.ActiveRelays = []string{"https://unknown.example"}
		return nil
	})
	if !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	after, err := (FileStore{Path: path}).Load()
	if err != nil || after.SettingsRevision() != saved.SettingsRevision() {
		t.Fatal("rejected transaction modified settings", err)
	}
}

func TestTransactionExcludesASeparateProcess(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("bounded test process owner requires Windows or Linux")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	owner, err := testprocess.BuildOwner(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	store := FileStore{Path: path}
	if err := store.Save(Config{Profiles: []senderrelay.Candidate{}}); err != nil {
		t.Fatal(err)
	}
	environment, err := testprocess.InheritEnvironment(map[string]string{transactionProbePath: path})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(ctx, func(current *Config) error {
		process, err := owner.Start(ctx, testprocess.Spec{
			Identity: testrun.Identity{RunID: "config-transaction", OperationID: "separate-process", Scenario: t.Name()},
			Command: testprocess.Command{Executable: executable, Arguments: []string{"-test.run=^TestTransactionChild$"},
				WorkingDirectory: root, Environment: environment},
			Deadline: 10 * time.Second, TerminationGrace: time.Second,
		})
		if err != nil {
			return err
		}
		result, err := process.Wait(ctx)
		if failure := testprocess.RequireSuccess(result, err); failure != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_, _ = process.Stop(cleanup)
			return fmt.Errorf("%w: %+v", failure, process.Stderr())
		}
		current.FrontURL = "https://parent.example"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransactionChild(t *testing.T) {
	path := os.Getenv(transactionProbePath)
	if path == "" {
		t.Skip("subprocess probe")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := (FileStore{Path: path}).Update(ctx, func(*Config) error {
		t.Error("entered a transaction while another process owned the lock")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock did not support bounded waiting:", err)
	}
}
