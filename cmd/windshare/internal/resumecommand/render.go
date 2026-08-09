package resumecommand

import (
	"errors"
	"fmt"
	"strings"

	"github.com/windshare/windshare/core/osfs"
)

type textRenderer struct{}

func (textRenderer) Usage() string {
	return "Usage:\n" +
		"  windshare resume list -o <directory>\n" +
		"      List current operation and terminal state through a fresh read-only inventory.\n\n" +
		"  windshare resume discard -o <directory> --item <N>\n" +
		"      Re-list one current item and require the exact terminal confirmation \"discard N\".\n" +
		"      Only owned recovery artifacts are eligible; published files are always preserved.\n\n" +
		"  windshare resume cleanup -o <directory>\n" +
		"      Run isolated legacy-state maintenance. It cannot discard current resume state.\n"
}

func (textRenderer) Inventory(items []resumeStateItem) (string, bool, error) {
	status := resumeListStatusReady
	for _, item := range items {
		if !item.valid() {
			return "", false, errResumeStateContract
		}
		if item.status == resumeItemStatusNeedsAttention {
			status = resumeListStatusNeedsAttention
		}
	}
	var output strings.Builder
	fmt.Fprintf(&output, "resume_list_status=%q items=%d\n", status, len(items))
	for index, item := range items {
		rendered, err := renderResumeStateItem(index+1, item)
		if err != nil {
			return "", false, err
		}
		output.WriteString(rendered)
	}
	return output.String(), status == resumeListStatusNeedsAttention, nil
}

func renderResumeStateItem(itemNumber int, item resumeStateItem) (string, error) {
	if itemNumber <= 0 || !item.valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_item=%d status=%q operation_id=%q intent_digest=%q phase=%d state_generation=%d expires_at_millis=%d success_count=%d failure_count=%d resumable=%t discardable=%t\n",
		itemNumber,
		item.status,
		item.operationID,
		item.intentDigest,
		item.phase,
		item.stateGeneration,
		item.expiresAtMillis,
		item.successCount,
		item.failureCount,
		item.resumable,
		item.discardable,
	)
	for _, attention := range item.attention {
		fmt.Fprintf(
			&output,
			"  resume_attention item=%d reason=%q operation_id=%q\n",
			itemNumber,
			attention.reason,
			attention.operationID,
		)
	}
	return output.String(), nil
}

func (textRenderer) DiscardPrompt(
	itemNumber int,
	item resumeStateItem,
	expected string,
) (string, error) {
	preview, err := renderResumeStateItem(itemNumber, item)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Current resume state:\n%sPublished files are preserved; only owned recovery artifacts are eligible.\nType %q exactly to continue: ",
		preview,
		expected,
	), nil
}

func (textRenderer) DiscardControlStatus(status string, itemNumber int) string {
	return fmt.Sprintf(
		"resume_discard_status=%q item=%d published_files=%q\n",
		status,
		itemNumber,
		resumePublishedFileTreatment,
	)
}

func (textRenderer) DiscardReport(itemNumber int, report resumeDiscardReport) (string, error) {
	if itemNumber <= 0 || !report.valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_discard_status=%q item=%d operation_id=%q phase=%d state_generation=%d resumable=%t published_files=%q\n",
		report.status,
		itemNumber,
		report.operationID,
		report.phase,
		report.stateGeneration,
		report.resumable,
		resumePublishedFileTreatment,
	)
	for _, attention := range report.attention {
		fmt.Fprintf(
			&output,
			"  resume_attention item=%d reason=%q operation_id=%q\n",
			itemNumber,
			attention.reason,
			attention.operationID,
		)
	}
	return output.String(), nil
}

func (textRenderer) Busy(operation string, itemNumber int, phase string) string {
	if operation == "discard" {
		return fmt.Sprintf(
			"resume_discard_status=%q item=%d phase=%q published_files=%q\n",
			resumeBusyStatus,
			itemNumber,
			phase,
			resumePublishedFileTreatment,
		)
	}
	return fmt.Sprintf("resume_list_status=%q phase=%q\n", resumeBusyStatus, phase)
}

func (textRenderer) LegacyCleanup(report osfs.CheckpointCleanupReport) (string, error) {
	if report.Status == 0 && !report.Complete && len(report.Attention) == 0 && len(report.Entries) == 0 {
		return "", errors.New("legacy cleanup report is empty")
	}
	statusName := checkpointCleanupStatusName(report.Status)
	if statusName == "unknown" {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"legacy_cleanup_status=%q complete=%t resumed=%t scanned=%d removed=%d quarantined=%d skipped=%d\n",
		statusName,
		report.Complete,
		report.Resumed,
		report.Scanned,
		report.Removed,
		report.Quarantined,
		report.Skipped,
	)
	for _, attention := range report.Attention {
		fmt.Fprintf(&output, "  legacy_attention=%q\n", attention)
	}
	for _, entry := range report.Entries {
		dispositionName := checkpointCleanupDispositionName(entry.Disposition)
		if dispositionName == "unknown" {
			return "", errResumeStateContract
		}
		fmt.Fprintf(
			&output,
			"  legacy_entry path=%q disposition=%q detail=%q\n",
			entry.RelativePath,
			dispositionName,
			entry.Detail,
		)
	}
	return output.String(), nil
}

func checkpointCleanupStatusName(status osfs.CheckpointCleanupStatus) string {
	switch status {
	case osfs.CheckpointCleanupStatusComplete:
		return "complete"
	case osfs.CheckpointCleanupStatusNeedsAttention:
		return "needs-attention"
	case osfs.CheckpointCleanupStatusInProgress:
		return "in-progress"
	default:
		return "unknown"
	}
}

func checkpointCleanupDispositionName(disposition osfs.CheckpointCleanupDisposition) string {
	switch disposition {
	case osfs.CheckpointCleanupSkip:
		return "skip"
	case osfs.CheckpointCleanupRemove:
		return "remove"
	case osfs.CheckpointCleanupQuarantine:
		return "quarantine"
	default:
		return "unknown"
	}
}
