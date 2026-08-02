package mutationdomain

import (
	"errors"
	"path/filepath"
)

const (
	maximumMutationInputObjects       = int64(2_000_000)
	maximumMutationInputBytes         = int64(32 << 30)
	maximumMutationInputMetadataBytes = int64(1 << 30)
	maximumMutationTreeDepth          = 256
	mutationTreeBatchSize             = 256
	mutationObjectMetadataOverhead    = int64(128)
)

type mutationTraversalLimits struct {
	objects       int64
	contentBytes  int64
	metadataBytes int64
	depth         int
}

type mutationTraversalBudget struct {
	limits        mutationTraversalLimits
	objects       int64
	contentBytes  int64
	metadataBytes int64
	deepest       int
}

func productionMutationTraversalBudget() *mutationTraversalBudget {
	return newMutationTraversalBudget(mutationTraversalLimits{
		objects:       maximumMutationInputObjects,
		contentBytes:  maximumMutationInputBytes,
		metadataBytes: maximumMutationInputMetadataBytes,
		depth:         maximumMutationTreeDepth,
	})
}

func newMutationTraversalBudget(limits mutationTraversalLimits) *mutationTraversalBudget {
	return &mutationTraversalBudget{limits: limits}
}

func (budget *mutationTraversalBudget) admitCandidate(name string) error {
	if name == "" || filepath.Base(name) != name {
		return errors.New("private mutation input candidate name is invalid")
	}
	return budget.admitObject(name, 0, 0)
}

func (budget *mutationTraversalBudget) admitObject(relative string, depth int, contentBytes int64) error {
	if budget == nil {
		return errors.New("private mutation traversal budget is unavailable")
	}
	if depth < 0 || depth > budget.limits.depth {
		return errors.New("private mutation input exceeded its directory depth bound")
	}
	metadataBytes := mutationObjectMetadataOverhead + int64(len(filepath.ToSlash(relative)))
	if contentBytes < 0 || exceedsMutationBudget(budget.contentBytes, contentBytes, budget.limits.contentBytes) {
		return errors.New("private mutation input exceeded its byte bound")
	}
	if exceedsMutationBudget(budget.objects, 1, budget.limits.objects) {
		return errors.New("private mutation input exceeded its object count bound")
	}
	if exceedsMutationBudget(budget.metadataBytes, metadataBytes, budget.limits.metadataBytes) {
		return errors.New("private mutation input exceeded its metadata byte bound")
	}
	budget.objects++
	budget.contentBytes += contentBytes
	budget.metadataBytes += metadataBytes
	if depth > budget.deepest {
		budget.deepest = depth
	}
	return nil
}

func exceedsMutationBudget(current, delta, maximum int64) bool {
	return current < 0 || delta < 0 || maximum < 0 || current > maximum || delta > maximum-current
}
