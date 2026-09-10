package destinationauthority

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const (
	processStageNonceBytes      = 32
	processStageDirectoryPrefix = ".windshare-live-"
	processStageFileName        = "partial"
	maximumProcessStageAttempts = 8
)

type processStageCreator interface {
	CreateProcessStage(outputcap.Directory, string, int64) (outputcap.MutableFile, error)
}

// ProcessStage owns only handles created during this process. Its random private
// directory is never reopened: after a crash its names are leftovers, not proof.
type ProcessStage struct {
	mu        sync.Mutex
	authority *BoundDestination
	directory outputcap.Directory
	file      outputcap.MutableFile
	name      string
	removed   bool
}

func (stage *ProcessStage) File() outputcap.MutableFile {
	if stage == nil {
		return nil
	}
	return stage.file
}

func (authority *BoundDestination) CreateProcessStage(
	ctx context.Context,
	parent LiveCleanupStageParent,
	exactSize uint64,
	random io.Reader,
) (*ProcessStage, error) {
	if authority == nil || ctx == nil || parent == nil || random == nil || exactSize > math.MaxInt64 {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stage *ProcessStage
	err := authority.withGuardedRoot(func(root outputcap.Directory) error {
		for range maximumProcessStageAttempts {
			var nonce [processStageNonceBytes]byte
			if _, err := io.ReadFull(random, nonce[:]); err != nil {
				return err
			}
			name := processStageDirectoryPrefix + hex.EncodeToString(nonce[:])
			directory, err := root.CreateDirectory(name, true)
			if errors.Is(err, outputcap.ErrNamespaceCollision) && directory == nil {
				continue
			}
			if err != nil || directory == nil {
				return errors.Join(ErrReservationIndeterminate, err, closeDirectory(directory))
			}
			stage = &ProcessStage{authority: authority, directory: directory, name: name}
			return nil
		}
		return ErrReservationExhausted
	})
	if err != nil {
		if stage != nil {
			return nil, errors.Join(err, stage.Close())
		}
		return nil, err
	}
	err = parent.WithExactParent(ctx, func(finalParent outputcap.Directory) error {
		creator, ok := finalParent.(processStageCreator)
		if !ok {
			return outputcap.ErrOrdinaryOutputUnsupported
		}
		var createErr error
		stage.file, createErr = creator.CreateProcessStage(stage.directory, processStageFileName, int64(exactSize))
		if createErr != nil || stage.file == nil {
			return errors.Join(ErrReservationIndeterminate, createErr)
		}
		size, sizeErr := stage.file.Size()
		if sizeErr != nil || size != exactSize {
			return errors.Join(ErrReservationIndeterminate, sizeErr)
		}
		return nil
	})
	if err != nil {
		return nil, errors.Join(err, stage.rollbackCreation(), stage.Close())
	}
	return stage, nil
}

// Remove removes only the retained native file and its exact owned container.
// Namespace replacement or unknown siblings stop cleanup and preserve evidence.
func (stage *ProcessStage) Remove() error {
	if stage == nil {
		return ErrInvalidConfiguration
	}
	stage.mu.Lock()
	defer stage.mu.Unlock()
	if stage.removed {
		return nil
	}
	if stage.file == nil || stage.directory == nil || stage.authority == nil {
		return ErrAuthorityClosed
	}
	if err := stage.directory.RemoveFile(processStageFileName, stage.file); err != nil {
		return err
	}
	if err := stage.authority.withGuardedRoot(func(root outputcap.Directory) error {
		return root.RemoveDirectory(stage.name, stage.directory)
	}); err != nil {
		return err
	}
	stage.removed = true
	return nil
}

func (stage *ProcessStage) Close() error {
	if stage == nil {
		return nil
	}
	stage.mu.Lock()
	defer stage.mu.Unlock()
	err := errors.Join(closeFile(stage.file), closeDirectory(stage.directory))
	stage.file, stage.directory, stage.authority = nil, nil, nil
	return err
}

func (stage *ProcessStage) rollbackCreation() error {
	// A returned file is a live identity witness even after a later create error.
	// Without it only nonrecursive removal of the owned empty container is safe.
	if stage.file != nil {
		return stage.Remove()
	}
	return stage.authority.withGuardedRoot(func(root outputcap.Directory) error {
		return root.RemoveDirectory(stage.name, stage.directory)
	})
}
