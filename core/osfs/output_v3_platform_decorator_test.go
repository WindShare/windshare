package osfs

// outputV3DecoratedPublicOperationGuard keeps the native guard's lifetime and
// placement authority while allowing fault and race decorators to observe the
// guarded root used by public-namespace operations.
type outputV3DecoratedPublicOperationGuard struct {
	outputV3PublicOperationGuard
	root outputV3Directory
}

func (guard *outputV3DecoratedPublicOperationGuard) Root() outputV3Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *outputV3DecoratedPublicOperationGuard) Close() error {
	if guard == nil {
		return nil
	}
	guard.root = nil
	if guard.outputV3PublicOperationGuard == nil {
		return nil
	}
	err := guard.outputV3PublicOperationGuard.Close()
	guard.outputV3PublicOperationGuard = nil
	return err
}

func acquireOutputV3DecoratedPublicOperationGuard(
	platform outputV3Platform,
	decorate func(outputV3Directory) outputV3Directory,
) (outputV3PublicOperationGuard, error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	return &outputV3DecoratedPublicOperationGuard{
		outputV3PublicOperationGuard: guard,
		root:                         decorate(guard.Root()),
	}, nil
}
