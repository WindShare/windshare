package dirpublish

import (
	"context"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRenameOnWindowsRetriesOnlyTransientPublicationErrors(t *testing.T) {
	denied := &os.LinkError{Op: "rename", Old: "staged", New: "published", Err: windows.ERROR_ACCESS_DENIED}
	sharing := &os.LinkError{Op: "rename", Old: "staged", New: "published", Err: windows.ERROR_SHARING_VIOLATION}
	missing := &os.LinkError{Op: "rename", Old: "staged", New: "published", Err: windows.ERROR_PATH_NOT_FOUND}
	for _, test := range []struct {
		name    string
		results []error
		cancel  bool
		want    error
	}{
		{name: "success", results: []error{nil}},
		{name: "access restored", results: []error{denied, nil}},
		{name: "sharing restored", results: []error{sharing, nil}},
		{name: "still denied", results: []error{denied, denied}, want: denied},
		{name: "second error preserved", results: []error{denied, missing}, want: missing},
		{name: "missing path", results: []error{missing}, want: missing},
		{name: "canceled after denial", results: []error{sharing}, cancel: true, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			err := renameOnWindows(ctx, func() error {
				if calls >= len(test.results) {
					t.Fatal("unexpected extra rename")
				}
				if test.cancel {
					cancel()
				}
				result := test.results[calls]
				calls++
				return result
			})
			if err != test.want || calls != len(test.results) {
				t.Fatalf("rename returned %v after %d calls, want %v after %d", err, calls, test.want, len(test.results))
			}
		})
	}
}
