package outputruntime

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type outcomeNativeFailure struct {
	class outputcap.NativeErrorClass
	text  string
}

func (failure outcomeNativeFailure) Error() string { return failure.text }
func (failure outcomeNativeFailure) NativeErrorClass() outputcap.NativeErrorClass {
	return failure.class
}

func TestFilesystemOutputDiagnosticFreezesPrimaryBeforeCleanupJoin(t *testing.T) {
	primaryCanary := outcomeNativeFailure{
		class: outputcap.NativeErrorAccessDenied,
		text:  "primary provider path content capability canary",
	}
	cleanupCanary := outcomeNativeFailure{
		class: outputcap.NativeErrorSharingViolation,
		text:  "cleanup provider path content capability canary",
	}
	primary := diagnoseFilesystemOutputFailure(
		FilesystemOutputFailureNativeDurability,
		primaryCanary,
	)
	joined := freezeFilesystemOutputFailure(primary, cleanupCanary)
	diagnostic, ok := FilesystemOutputDiagnosticFor(joined)
	if !ok || diagnostic.Stage != FilesystemOutputFailureNativeDurability ||
		diagnostic.NativeErrorClass != FilesystemNativeErrorAccessDenied {
		t.Fatalf("joined diagnostic = (%+v, %t)", diagnostic, ok)
	}
	if !errors.Is(joined, primaryCanary) || !errors.Is(joined, cleanupCanary) {
		t.Fatalf("joined cause identity was lost: %v", joined)
	}
	for _, safe := range []string{
		diagnostic.Stage.String(),
		diagnostic.NativeErrorClass.String(),
		joined.Error(),
	} {
		if strings.Contains(safe, "provider") || strings.Contains(safe, "path") ||
			strings.Contains(safe, "content") || strings.Contains(safe, "capability") {
			t.Fatalf("public diagnostic leaked provider evidence: %q", safe)
		}
	}
}

func TestFilesystemOutputClosedVocabulariesRejectUnknownValues(t *testing.T) {
	if FilesystemOutputFailureStage(0).Valid() ||
		FilesystemOutputFailureStage(255).Valid() ||
		FilesystemCheckpointReconciliationStep(0).Valid() ||
		FilesystemCheckpointReconciliationStep(255).Valid() ||
		FilesystemNativeErrorClass(0).Valid() ||
		FilesystemNativeErrorClass(255).Valid() ||
		FilesystemOutputStateReason(0).Valid() ||
		FilesystemOutputStateReason(255).Valid() {
		t.Fatal("unknown filesystem outcome value escaped its closed vocabulary")
	}
}
