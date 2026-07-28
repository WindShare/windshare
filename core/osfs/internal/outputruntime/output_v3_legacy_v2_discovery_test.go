package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ListsAndDiscardsLegacyV2JournalWithoutRemovingItsStage(t *testing.T) {
	root := v3RecoveryRoot(t)
	journalName := legacyOutputStatePrefix + strings.Repeat("11", transfer.OutputSessionIdentityBytes) + legacyOutputJournalSuffix
	stageName := legacyOutputStagePrefix + strings.Repeat("22", 16)
	journalBytes := v3RecoveryLegacyV2JournalBytes(t)
	stageBytes := bytes.Repeat([]byte{0x5a}, 32)
	journalPath := filepath.Join(root, journalName)
	stagePath := filepath.Join(root, stageName)
	if err := os.WriteFile(journalPath, journalBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagePath, stageBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.Chmod(journalPath, 0o644), os.Chmod(stagePath, 0o644)); err != nil {
		t.Fatal(err)
	}

	authority := v3RecoveryAuthority(t, root, nil)
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 2 {
		t.Fatalf("legacy summaries = %+v, want journal and stage", summaries)
	}
	var journal, stage *ResumeStateSummary
	for index := range summaries {
		summary := &summaries[index]
		if summary.Reference.legacyName == journalName {
			journal = summary
		}
		if summary.Reference.legacyName == stageName {
			stage = summary
		}
	}
	if journal == nil || journal.Reference.Kind() != ResumeStateLegacyUntrusted ||
		!runtimeLegacyHasAttention(*journal, "legacy-v2-untrusted") {
		t.Fatalf("legacy journal summary = %+v", journal)
	}
	if stage == nil || stage.Reference.Kind() != ResumeStateLegacyUntrusted ||
		!runtimeLegacyHasAttention(*stage, "legacy-v2-stage-manual") {
		t.Fatalf("legacy stage summary = %+v", stage)
	}
	if _, err := authority.DiscardResumeState(context.Background(), stage.Reference); err == nil ||
		!errors.Is(err, outputfault.ErrLegacyState) || v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
		t.Fatalf("legacy stage discard error = %v, want root-scoped manual-retention fault", err)
	}
	if actual, err := os.ReadFile(stagePath); err != nil || !bytes.Equal(actual, stageBytes) {
		t.Fatalf("legacy stage changed by refused discard: bytes=%x err=%v", actual, err)
	}

	settlement, err := authority.DiscardResumeState(context.Background(), journal.Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("legacy journal discard = (%+v, %v)", settlement, err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded legacy journal stat error = %v, want not exist", err)
	}
	if actual, err := os.ReadFile(stagePath); err != nil || !bytes.Equal(actual, stageBytes) {
		t.Fatalf("journal discard removed or changed stage: bytes=%x err=%v", actual, err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}

	remainingInventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, remainingInventory)
	remaining := remainingInventory.Summaries()
	if len(remaining) != 1 || remaining[0].Reference.legacyName != stageName {
		t.Fatalf("remaining legacy state = %+v, want only manual stage", remaining)
	}
}

func TestOutputV3LegacyV2DiscardRejectsChangedAuthority(t *testing.T) {
	t.Run("journal-content", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		authority, inventory, summary, journalPath, journalBytes := v3RecoveryListedLegacyV2Journal(t, root)
		defer v3RecoveryCloseInventory(t, inventory)
		changed := bytes.Clone(journalBytes)
		changed[len(outputJournalMagicV2)] ^= 0x01
		if err := os.WriteFile(journalPath, changed, 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := authority.DiscardResumeState(context.Background(), summary.Reference); err == nil ||
			!errors.Is(err, outputfault.ErrRootUnsafe) || v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
			t.Fatalf("discard changed legacy journal error = %v, want root-scoped digest rejection", err)
		}
		if actual, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(actual, changed) {
			t.Fatalf("changed legacy journal mutated by refused discard: bytes=%x err=%v", actual, err)
		}
	})

	t.Run("journal-leaf", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		authority, inventory, summary, journalPath, journalBytes := v3RecoveryListedLegacyV2Journal(t, root)
		defer v3RecoveryCloseInventory(t, inventory)
		movedPath := journalPath + ".listed"
		if err := os.Rename(journalPath, movedPath); err != nil {
			t.Fatal(err)
		}
		// Identical replacement bytes deliberately satisfy the recorded size and
		// digest. Only the exact live entry pin may authorize deletion here.
		if err := os.WriteFile(journalPath, journalBytes, 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := authority.DiscardResumeState(context.Background(), summary.Reference); err == nil ||
			!errors.Is(err, outputfault.ErrRootUnsafe) || v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
			t.Fatalf("discard replacement legacy leaf error = %v, want root-scoped identity rejection", err)
		}
		if actual, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(actual, journalBytes) {
			t.Fatalf("replacement legacy leaf mutated by refused discard: bytes=%x err=%v", actual, err)
		}
	})

	t.Run("output-root-incarnation", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		authority, inventory, summary, journalPath, journalBytes := v3RecoveryListedLegacyV2Journal(t, root)
		defer v3RecoveryCloseInventory(t, inventory)
		movedRoot := root + "-listed"
		t.Cleanup(func() { _ = os.RemoveAll(movedRoot) })
		if err := os.Rename(root, movedRoot); err != nil {
			// NTFS denies ancestor replacement while the inventory's root handle is
			// live. That kernel-enforced pin is stronger than detecting a replacement
			// during discard; POSIX still exercises the post-replacement rejection.
			if runtimeDiscardIsBlockedAncestorReplacement(err) {
				if actual, readErr := os.ReadFile(journalPath); readErr != nil || !bytes.Equal(actual, journalBytes) {
					t.Fatalf("failed root replacement changed the pinned journal: bytes=%x err=%v", actual, readErr)
				}
				return
			}
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(journalPath, journalBytes, 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := authority.DiscardResumeState(context.Background(), summary.Reference); err == nil ||
			!errors.Is(err, outputfault.ErrRootUnsafe) || v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
			t.Fatalf("discard replacement output root error = %v, want root-binding rejection", err)
		}
		if actual, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(actual, journalBytes) {
			t.Fatalf("replacement-root journal mutated by refused discard: bytes=%x err=%v", actual, err)
		}
	})
}

func TestOutputV3LegacyInventorySurvivesUnsupportedV3Platform(t *testing.T) {
	root := v3RecoveryRoot(t)
	journalName := legacyOutputStatePrefix + strings.Repeat("11", transfer.OutputSessionIdentityBytes) + legacyOutputJournalSuffix
	stageName := legacyOutputStagePrefix + strings.Repeat("22", 16)
	journalBytes := v3RecoveryLegacyV2JournalBytes(t)
	stageBytes := bytes.Repeat([]byte{0x6b}, 32)
	journalPath := filepath.Join(root, journalName)
	stagePath := filepath.Join(root, stageName)
	if err := errors.Join(
		os.WriteFile(journalPath, journalBytes, 0o644),
		os.WriteFile(stagePath, stageBytes, 0o644),
		os.Chmod(journalPath, 0o644),
		os.Chmod(stagePath, 0o644),
	); err != nil {
		t.Fatal(err)
	}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.platformFactory = func(string, bool) (outputcap.Platform, error) {
		return nil, outputcap.ErrRecoverableOutputUnsupported
	}
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 2 {
		t.Fatalf("portable legacy summaries = %+v, want journal and stage", summaries)
	}
	var journalReference ResumeStateRef
	for _, summary := range summaries {
		if summary.Reference.legacyName == journalName {
			journalReference = summary.Reference
		}
	}
	if journalReference.Kind() != ResumeStateLegacyUntrusted {
		t.Fatalf("portable legacy journal reference = %+v", journalReference)
	}
	if _, err := authority.DiscardResumeState(context.Background(), journalReference); err == nil ||
		!errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) || v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
		t.Fatalf("unsupported-platform legacy discard error = %v, want typed root fault", err)
	}
	if actual, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(actual, journalBytes) {
		t.Fatalf("unsupported discard changed journal: bytes=%x err=%v", actual, err)
	}
	if actual, err := os.ReadFile(stagePath); err != nil || !bytes.Equal(actual, stageBytes) {
		t.Fatalf("unsupported discard changed stage: bytes=%x err=%v", actual, err)
	}
}

const legacyV2FilesystemBackend = "windshare/cli-osfs/v2"

var outputJournalMagicV2 = [8]byte{'W', 'S', 'O', 'U', 'T', 'P', 'U', 'T'}

type v3RecoveryLegacyV2Journal struct {
	Schema        uint32                          `json:"schema"`
	Backend       string                          `json:"backend"`
	OutputSession string                          `json:"outputSession"`
	ShareInstance string                          `json:"shareInstance"`
	ResumeIntent  string                          `json:"resumeIntent"`
	RootLocator   string                          `json:"rootLocator"`
	RootIdentity  string                          `json:"rootIdentity"`
	Files         []v3RecoveryLegacyV2JournalFile `json:"files"`
}

type v3RecoveryLegacyV2JournalFile struct{}

func v3RecoveryLegacyV2JournalBytes(t *testing.T) []byte {
	t.Helper()
	encode := func(value byte, size int) string {
		return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, size))
	}
	payload, err := json.Marshal(v3RecoveryLegacyV2Journal{
		Schema: 2, Backend: legacyV2FilesystemBackend,
		OutputSession: encode(0x11, transfer.OutputSessionIdentityBytes),
		ShareInstance: encode(0x31, catalog.IdentityBytes),
		ResumeIntent:  encode(0x32, sha256.Size),
		RootLocator:   encode(0x33, sha256.Size),
		RootIdentity:  encode(0x34, sha256.Size),
		Files:         make([]v3RecoveryLegacyV2JournalFile, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := make([]byte, 0, len(outputJournalMagicV2)+len(payload)+sha256.Size)
	encoded = append(encoded, outputJournalMagicV2[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...)
}

func v3RecoveryListedLegacyV2Journal(
	t *testing.T,
	root string,
) (*Authority, *ResumeStateInventory, ResumeStateSummary, string, []byte) {
	t.Helper()
	journalName := legacyOutputStatePrefix + strings.Repeat("11", transfer.OutputSessionIdentityBytes) + legacyOutputJournalSuffix
	journalPath := filepath.Join(root, journalName)
	journalBytes := v3RecoveryLegacyV2JournalBytes(t)
	if err := os.WriteFile(journalPath, journalBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journalPath, 0o644); err != nil {
		t.Fatal(err)
	}
	authority := v3RecoveryAuthority(t, root, nil)
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Reference.legacyName != journalName ||
		runtimeLegacyHasAttention(summaries[0], "legacy-v2-journal-unreadable") {
		_ = inventory.Close()
		t.Fatalf("list removable v2 journal = %+v", summaries)
	}
	return authority, inventory, summaries[0], journalPath, journalBytes
}

func runtimeLegacyHasAttention(summary ResumeStateSummary, expected string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == expected {
			return true
		}
	}
	return false
}
