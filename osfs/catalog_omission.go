package osfs

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	diag "github.com/windshare/windshare/core/diagnostics"
)

type catalogOmissions struct {
	count   uint64
	samples []catalog.SourceOmission
}

func catalogOmissionFor(info fs.FileInfo) catalog.SourceOmissionReason {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return catalog.SourceOmissionSymbolicLink
	case isReparsePoint(info):
		return catalog.SourceOmissionReparsePoint
	case !info.IsDir() && !info.Mode().IsRegular():
		return catalog.SourceOmissionSpecialFile
	default:
		return catalog.SourceOmissionNone
	}
}

// A denied child must not hide readable siblings. Permission fixes take effect
// in a new share because committed directory generations are immutable.
func classifyChildAccessError(name string, err error) (catalog.SourceOmission, error) {
	if errors.Is(err, fs.ErrPermission) {
		return catalog.SourceOmission{Name: name, Reason: catalog.SourceOmissionAccessDenied}, nil
	}
	return catalog.SourceOmission{}, catalog.NewTransientScanError(err, 0)
}

func (source *SelectedCatalogSource) newScanOmissionReporter(
	request catalog.ScanRequest,
	parent sourceLocation,
	directoryPath string,
	initial catalogOmissions,
) catalog.ScanOmissionReporter {
	if source.omissions == nil && !source.diagnostics.Enabled() {
		return nil
	}
	var once sync.Once
	return catalog.ScanOmissionReporterFunc(func(additionalCount uint64, additionalSamples []catalog.SourceOmission) {
		once.Do(func() {
			totalCount := initial.count + additionalCount
			if totalCount == 0 {
				return
			}
			samples := make(
				[]catalog.SourceOmission,
				0,
				min(len(initial.samples)+len(additionalSamples), catalog.MaxSourceOmissionSamples),
			)
			samples = append(samples, initial.samples...)
			if remaining := catalog.MaxSourceOmissionSamples - len(samples); remaining > 0 && len(additionalSamples) > 0 {
				samples = append(samples, additionalSamples[:min(len(additionalSamples), remaining)]...)
			}
			for index, sample := range samples {
				source.traceCatalogOmission(request, parent, sample, uint64(index+1))
			}
			source.traceCatalogOmissionSummary(request, totalCount)
			source.observeCatalogOmissions(request, directoryPath, catalogOmissions{
				count:   totalCount,
				samples: samples,
			})
		})
	})
}

func (source *SelectedCatalogSource) observeCatalogOmissions(request catalog.ScanRequest, path string, result catalogOmissions) {
	if source.omissions == nil || result.count == 0 {
		return
	}
	directory, _ := request.Directory.DirectoryID()
	source.omissions.ObserveSourceOmissions(catalog.SourceOmissions{
		Directory: directory, Generation: request.Generation, Attempt: request.AttemptID,
		DirectoryPath: path, Count: result.count, Samples: result.samples,
	})
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
	request catalog.ScanRequest, parent sourceLocation, omission catalog.SourceOmission, count uint64,
) {
	if !source.diagnostics.Enabled() || count > catalog.MaxSourceOmissionSamples {
		return
	}
	source.diagnostics.Emit(diag.New("catalog_source_omission", diag.Fields{
		"root_slot":     diag.Uint(uint64(parent.RootSlot())),
		"relative_path": diag.Text(filepath.ToSlash(filepath.Join(parent.RelativePath(), omission.Name))),
		"reason":        diag.Text(string(omission.Reason)),
		"detail":        diag.Text(omission.Detail),
	}).Correlate(catalogScanCorrelation(request)))
}

func (source *SelectedCatalogSource) traceCatalogOmissionSummary(request catalog.ScanRequest, count uint64) {
	if count == 0 || !source.diagnostics.Enabled() {
		return
	}
	samples := min(count, catalog.MaxSourceOmissionSamples)
	source.diagnostics.Emit(diag.New("catalog_source_omission_summary", diag.Fields{
		"omitted_entries": diag.Uint(count), "sampled_entries": diag.Uint(samples),
		"unsampled_entries": diag.Uint(count - samples),
	}).Correlate(catalogScanCorrelation(request)))
}
