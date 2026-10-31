package namestore

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputnames"
)

const maximumReportPageBytes = 1024 * 1024

type reportEntry struct {
	OriginalPath string `json:"originalPath"`
	SavedPath    string `json:"savedPath"`
}

type reportPage struct {
	Format       string        `json:"format"`
	OperationID  string        `json:"operationId"`
	OriginalRoot string        `json:"originalRoot"`
	SavedRoot    string        `json:"savedRoot"`
	Page         int           `json:"page"`
	Entries      []reportEntry `json:"entries"`
}

type nameView interface {
	Mappings() []outputnames.Mapping
	Resolve(string) (string, error)
}

// Export keeps the source spellings readable after private resume state is
// retired. All saved paths are relative to the selected destination container;
// the report lives beside the result, so it does not change source directory metadata.
func (store *Store) Export(mapper nameView, root outputnames.Root, visit func(func(outputcap.Directory) error) error) error {
	mappings := mapper.Mappings()
	if len(mappings) == 0 && root.ReservedName == root.PhysicalName {
		return nil
	}
	return visit(func(directory outputcap.Directory) error {
		return store.exportTo(directory, mapper, root, mappings)
	})
}

func (store *Store) exportTo(directory outputcap.Directory, mapper nameView, root outputnames.Root, mappings []outputnames.Mapping) error {
	page := reportPage{
		Format: "windshare/native-name-report/v1", OperationID: store.operationID,
		OriginalRoot: root.RequestedName, SavedRoot: root.PhysicalName, Page: 1,
		Entries: []reportEntry{},
	}
	pageBytes := 0
	flush := func() error {
		encoded, err := json.MarshalIndent(page, "", "  ")
		if err != nil {
			return err
		}
		if err := installReport(directory, store.operationID, page.Page, append(encoded, '\n')); err != nil {
			return err
		}
		page.Page++
		page.Entries = []reportEntry{}
		pageBytes = 0
		return nil
	}
	for _, mapping := range mappings {
		physical, err := mapper.Resolve(mapping.LogicalPath)
		if err != nil {
			return err
		}
		entry := reportEntry{OriginalPath: mapping.LogicalPath, SavedPath: root.PhysicalName + "/" + physical}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if pageBytes+len(encoded) > maximumReportPageBytes && len(page.Entries) > 0 {
			if err := flush(); err != nil {
				return err
			}
		}
		page.Entries = append(page.Entries, entry)
		pageBytes += len(encoded)
	}
	return flush()
}

func installReport(directory outputcap.Directory, operation string, page int, encoded []byte) error {
	for attempt := range outputnames.MaximumAttempts {
		name := fmt.Sprintf("windshare-names-%s-%d-%d.json", operation, page, attempt)
		kind, exact, err := directory.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if kind != outputcap.EntryAbsent {
			if kind != outputcap.EntryRegularFile || !exact {
				continue
			}
			existing, err := checkpointstore.ReadFile(directory, name)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, encoded) {
				continue
			}
		}
		// The installer publishes without replacement and accepts only identical
		// bytes on retry, so a source file or edited report is never overwritten.
		return checkpointstore.InstallCreate(directory, name, encoded)
	}
	return outputnames.ErrInvalidMapping
}
