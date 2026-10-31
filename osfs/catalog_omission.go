package osfs

import (
	"io/fs"
	"path/filepath"

	"github.com/windshare/windshare/core/catalog"
	diag "github.com/windshare/windshare/core/diagnostics"
)

type catalogOmissionReason string

const (
	catalogOmissionNone    catalogOmissionReason = ""
	catalogOmissionSymlink catalogOmissionReason = "symbolic_link"
	catalogOmissionReparse catalogOmissionReason = "reparse_point"
	catalogOmissionSpecial catalogOmissionReason = "special_file"
	// Path samples bound diagnostic work even for dependency trees full of links.
	maxCatalogOmissionSamples = uint64(16)
)

func catalogOmissionFor(info fs.FileInfo) catalogOmissionReason {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return catalogOmissionSymlink
	case isReparsePoint(info):
		return catalogOmissionReparse
	case !info.IsDir() && !info.Mode().IsRegular():
		return catalogOmissionSpecial
	default:
		return catalogOmissionNone
	}
}

func catalogScanCorrelation(request catalog.ScanRequest) diag.Fields {
	directory, _ := request.Directory.DirectoryID()
	return diag.Fields{
		"directory_id":    diag.Identity(directory.Bytes()),
		"generation_id":   diag.Identity(request.Generation.Bytes()),
		"scan_attempt_id": diag.Identity(request.AttemptID.Bytes()),
	}
}

func (source *SelectedCatalogSource) traceCatalogOmission(
	request catalog.ScanRequest, parent sourceLocation, name string, reason catalogOmissionReason, count uint64,
) {
	if !source.diagnostics.Enabled() || count > maxCatalogOmissionSamples {
		return
	}
	source.diagnostics.Emit(diag.New("catalog_source_omission", diag.Fields{
		"root_slot":     diag.Uint(uint64(parent.RootSlot())),
		"relative_path": diag.Text(filepath.ToSlash(filepath.Join(parent.RelativePath(), name))),
		"reason":        diag.Text(string(reason)),
	}).Correlate(catalogScanCorrelation(request)))
}

func (source *SelectedCatalogSource) traceCatalogOmissionSummary(request catalog.ScanRequest, count uint64) {
	if count == 0 || !source.diagnostics.Enabled() {
		return
	}
	samples := min(count, maxCatalogOmissionSamples)
	source.diagnostics.Emit(diag.New("catalog_source_omission_summary", diag.Fields{
		"omitted_entries": diag.Uint(count), "sampled_entries": diag.Uint(samples),
		"unsampled_entries": diag.Uint(count - samples),
	}).Correlate(catalogScanCorrelation(request)))
}
