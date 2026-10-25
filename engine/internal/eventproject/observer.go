package eventproject

import (
	liveshare "github.com/windshare/windshare/core/liveshare"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectSharingSubject(summary liveshare.SelectedRootSummary) (appobs.SharingSubjectSelected, error) {
	count := summary.SelectedCount()
	if count == 0 {
		return appobs.SharingSubjectSelected{}, ErrInvalidProjection
	}
	var subject appobs.SharingSubject
	var err error
	if count > 1 {
		subject, err = appobs.NewMultipleSubject(count)
	} else {
		root, ok := summary.SingleRoot()
		if !ok || root.Name() == "" {
			return appobs.SharingSubjectSelected{}, ErrInvalidProjection
		}
		name := appobs.NewDisplayName(root.Name())
		switch root.Kind() {
		case liveshare.SelectedRootKindFile:
			size, ok := root.FileSize()
			if !ok {
				return appobs.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = appobs.NewFileSubject(name, size)
		case liveshare.SelectedRootKindDirectory:
			if _, ok := root.FileSize(); ok {
				return appobs.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = appobs.NewDirectorySubject(name)
		default:
			return appobs.SharingSubjectSelected{}, ErrInvalidProjection
		}
	}
	if err != nil {
		return appobs.SharingSubjectSelected{}, ErrInvalidProjection
	}
	event, err := appobs.NewSharingSubjectSelected(subject)
	if err != nil {
		return appobs.SharingSubjectSelected{}, ErrInvalidProjection
	}
	return event, nil
}
