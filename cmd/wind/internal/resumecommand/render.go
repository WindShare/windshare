package resumecommand

import (
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
	if !snapshot.valid() {
		return "", false, errResumeStateContract
	}
	status := resumeListStatusReady
	if snapshot.needsAttention() {
		status = resumeListStatusNeedsAttention
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_list_status=%q operations=%d registry_unknown=%t\n",
		status,
		len(snapshot.operations),
		snapshot.registryUnknown,
	)
	for index, operation := range snapshot.operations {
		rendered, err := renderResumeOperation(index+1, operation)
		if err != nil {
			return "", false, err
		}
		output.WriteString(rendered)
	}
	return output.String(), snapshot.needsAttention(), nil
}

func (textRenderer) ListControlStatus(
	status string,
	reason string,
	detail resumeFailureDetail,
) (string, error) {
	if !detail.valid() {
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
	fmt.Fprintf(output, " stage=%q", detail.stage.String())
	if detail.reconciliation != 0 {
		fmt.Fprintf(output, " reconciliation_stage=%q", detail.reconciliation.String())
	}
	if detail.nativeClass != 0 {
		fmt.Fprintf(output, " native_error_class=%q", detail.nativeClass.String())
	}
}

func renderResumeOperation(itemNumber int, operation resumeOperation) (string, error) {
	if itemNumber <= 0 || !operation.valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_operation=%d state=%q operation_id=%q running=%t item-blocked=%d",
		itemNumber,
		operation.state.String(),
		operation.operationID,
		operation.running,
		len(operation.blockedItems),
	)
	if operation.attention != "" {
		fmt.Fprintf(&output, " reason=%q", operation.attention)
	}
	output.WriteByte('\n')
	for _, item := range operation.blockedItems {
		if item.pathKnown {
			fmt.Fprintf(
				&output,
				"  item-blocked path=%q reason=%q\n",
				item.artifactPath,
				item.reason.String(),
			)
			continue
		}
		fmt.Fprintf(
			&output,
			"  item-blocked path_known=false reason=%q\n",
			item.reason.String(),
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
	if !detail.valid() {
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
	if itemNumber <= 0 || !report.valid() {
		return "", errResumeStateContract
	}
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"resume_discard_status=%q item=%d operation_id=%q published_files=%q foreign_objects=%q",
		report.status,
		itemNumber,
		report.operationID,
		resumePublishedFileTreatment,
		resumeForeignObjectTreatment,
	)
	if report.attention != "" {
		fmt.Fprintf(&output, " reason=%q", report.attention)
	}
	output.WriteByte('\n')
	for _, item := range report.blockedItems {
		if item.pathKnown {
			fmt.Fprintf(
				&output,
				"  item-blocked path=%q reason=%q\n",
				item.artifactPath,
				item.reason.String(),
			)
			continue
		}
		fmt.Fprintf(
			&output,
			"  item-blocked path_known=false reason=%q\n",
			item.reason.String(),
		)
	}
	return output.String(), nil
}
