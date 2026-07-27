//go:build linux || windows

package cli

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testoutputroot"
)

func TestFilesystemResumeInventoryForwardsExactCoreReference(t *testing.T) {
	fixture := testoutputroot.New(t)
	authority, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath:   fixture.RootPath,
		CreateRoot: fixture.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []byte{0x11, 0x44} {
		pauseResumeReferenceTestSession(t, authority, resumeReferenceTestSelection(t, seed))
	}

	coreInventory, err := osfs.ListResumeState(
		context.Background(),
		osfs.FilesystemResumeRoot{RootPath: fixture.RootPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	summaries := coreInventory.Summaries()
	if len(summaries) != 2 {
		_ = coreInventory.Close()
		t.Fatalf("summaries=%+v", summaries)
	}
	first := summaries[0].Reference
	expected := summaries[1].Reference
	if first == (osfs.ResumeStateRef{}) ||
		expected == (osfs.ResumeStateRef{}) || expected == first {
		_ = coreInventory.Close()
		t.Fatal("live inventory did not return distinct nonempty references")
	}

	var forwarded osfs.ResumeStateRef
	adapter := newFilesystemResumeStateInventory(
		coreInventory,
		summaries,
		func(_ context.Context, reference osfs.ResumeStateRef) (osfs.DiscardSettlement, error) {
			forwarded = reference
			return osfs.DiscardSettlement{Kind: osfs.Discarded}, nil
		},
	)
	items := adapter.Items()
	if _, err := adapter.Discard(context.Background(), items[1]); err != nil {
		_ = adapter.Close()
		t.Fatal(err)
	}
	if forwarded != expected {
		_ = adapter.Close()
		t.Fatal("adapter reconstructed or substituted the selected live core reference")
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
}

func pauseResumeReferenceTestSession(
	t *testing.T,
	authority transfer.OutputAuthority,
	selection transfer.OutputSelection,
) {
	t.Helper()
	session, err := authority.OpenSelection(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	paused := false
	defer func() {
		if !paused {
			if _, pauseErr := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); pauseErr != nil {
				t.Errorf("release failed resume-reference session: %v", pauseErr)
			}
		}
	}()
	if _, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	paused = true
}

func resumeReferenceTestSelection(t *testing.T, seed byte) transfer.OutputSelection {
	t.Helper()
	share := resumeReferenceTestIdentity[catalog.ShareInstance](seed)
	root := resumeReferenceTestIdentity[catalog.DirectoryID](seed + 1)
	generation := resumeReferenceTestIdentity[catalog.DirectoryGeneration](seed + 2)
	plan, err := transfer.NewOutputSelection(share, root, generation, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func resumeReferenceTestIdentity[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}
