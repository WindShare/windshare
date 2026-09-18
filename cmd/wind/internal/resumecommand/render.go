package resumecommand

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/commandmeta"
)

type textRenderer struct{}

func (textRenderer) Usage() string {
	return "Usage:\n" +
		"  " + commandmeta.Name + " resume list -o <directory>\n" +
		"      List unfinished operations owned by that output directory.\n\n" +
		"  " + commandmeta.Name + " resume discard -o <directory> --item <N>\n" +
		"      Re-list one operation and require the exact confirmation \"discard N\".\n" +
		"      Only identity-matched unfinished state is removed; final and foreign objects stay.\n\n" +
		"States:\n" +
		"  incomplete                 Unfinished, with no currently usable owned partial.\n" +
		"  resumable                  At least one verified owned partial can continue.\n" +
		"  cleanup-pending            Transfer ended, but exact owned cleanup remains.\n" +
		"  operation-needs-attention  Root, registry, lease, or operation ownership is uncertain.\n" +
		"  item-blocked               A child cannot be resolved safely; its objects stay.\n" +
		"  running=true               Another process holds the operation; details are not inspected.\n" +
		"  registry_unknown=true      Registry ownership is incomplete, so discard is disabled.\n"
}

func (textRenderer) Inventory(snapshot resumeInventorySnapshot) (string, bool, error) {
	if !snapshot.Valid() {
		return "", false, errResumeStateContract
	}
	status := resumeListStatusReady
	if snapshot.NeedsAttention() {
		status = resumeListStatusNeedsAttention
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_list_status=%q operations=%d registry_unknown=%t\n",
		status,
		len(snapshot.Operations),
		snapshot.RegistryUnknown,
	)
	for index, operation := range snapshot.Operations {
		rendered, err := renderResumeOperation(index+1, operation)
		if err != nil {
			return "", false, err
		}
		output.WriteString(rendered)
	}
	return output.String(), snapshot.NeedsAttention(), nil
}

func (textRenderer) ListControlStatus(
	status string,
	reason string,
	detail resumeFailureDetail,
) (string, error) {
	if !detail.Valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(&output, "resume_list_status=%q reason=%q", status, reason)
	renderResumeFailureDetail(&output, detail)
	output.WriteByte('\n')
	return output.String(), nil
}

func renderResumeFailureDetail(output *strings.Builder, detail resumeFailureDetail) {
	if output == nil || detail == (resumeFailureDetail{}) {
		return
	}
	fmt.Fprintf(output, " stage=%q", detail.Stage.String())
	if detail.Reconciliation != 0 {
		fmt.Fprintf(output, " reconciliation_stage=%q", detail.Reconciliation.String())
	}
	if detail.NativeClass != 0 {
		fmt.Fprintf(output, " native_error_class=%q", detail.NativeClass.String())
	}
}

func renderResumeOperation(itemNumber int, operation resumeOperation) (string, error) {
	if itemNumber <= 0 || !operation.Valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_operation=%d state=%q operation_id=%q running=%t item-blocked=%d",
		itemNumber,
		operation.State.String(),
		hex.EncodeToString(operation.ID.Bytes()),
		operation.Running,
		len(operation.BlockedItems),
	)
	if operation.Attention != "" {
		fmt.Fprintf(&output, " reason=%q", operation.Attention)
	}
	output.WriteByte('\n')
	for _, item := range operation.BlockedItems {
		if item.PathKnown {
			fmt.Fprintf(
				&output,
				"  item-blocked path=%q reason=%q\n",
				item.ArtifactPath,
				item.Reason.String(),
			)
			continue
		}
		fmt.Fprintf(
			&output,
			"  item-blocked path_known=false reason=%q\n",
			item.Reason.String(),
		)
	}
	return output.String(), nil
}

func (textRenderer) DiscardPrompt(
	itemNumber int,
	operation resumeOperation,
	expected string,
) (string, error) {
	preview, err := renderResumeOperation(itemNumber, operation)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Selected resume operation:\n%sOnly identity-matched unfinished partial and control records are eligible. Final and foreign objects are preserved.\nType %q exactly to continue: ",
		preview,
		expected,
	), nil
}

func (textRenderer) DiscardControlStatus(
	status string,
	itemNumber int,
	reason string,
	detail resumeFailureDetail,
) (string, error) {
	if !detail.Valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_discard_status=%q item=%d reason=%q published_files=%q foreign_objects=%q",
		status,
		itemNumber,
		reason,
		resumePublishedFileTreatment,
		resumeForeignObjectTreatment,
	)
	renderResumeFailureDetail(&output, detail)
	output.WriteByte('\n')
	return output.String(), nil
}

func (textRenderer) DiscardReport(itemNumber int, report resumeDiscardReport) (string, error) {
	if itemNumber <= 0 || !report.Valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_discard_status=%q item=%d operation_id=%q published_files=%q foreign_objects=%q",
		report.Status,
		itemNumber,
		hex.EncodeToString(report.ID.Bytes()),
		resumePublishedFileTreatment,
		resumeForeignObjectTreatment,
	)
	if report.Attention != "" {
		fmt.Fprintf(&output, " reason=%q", report.Attention)
	}
	output.WriteByte('\n')
	for _, item := range report.BlockedItems {
		if item.PathKnown {
			fmt.Fprintf(
				&output,
				"  item-blocked path=%q reason=%q\n",
				item.ArtifactPath,
				item.Reason.String(),
			)
			continue
		}
		fmt.Fprintf(
			&output,
			"  item-blocked path_known=false reason=%q\n",
			item.Reason.String(),
		)
	}
	return output.String(), nil
}
