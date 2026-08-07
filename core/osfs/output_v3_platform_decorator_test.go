//go:build windows

package osfs

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
)

// newOutputV3DecoratedPublicAuthority keeps native fault injection at the
// capability boundary while every test interaction still crosses the public
// FilesystemOutputAuthority facade.
func newOutputV3DecoratedPublicAuthority(
	t *testing.T,
	rootPath string,
	decorate func(outputcap.Platform) outputcap.Platform,
) *FilesystemOutputAuthority {
	t.Helper()
	runtimeAuthority, err := outputruntime.New(outputruntime.Config{
		RootPath: rootPath,
		PlatformFactory: func(path string, create bool) (outputcap.Platform, error) {
			platform, openErr := openNativeOutputPlatform(path, create)
			if openErr != nil {
				return nil, openErr
			}
			if decorate == nil {
				return platform, nil
			}
			return decorate(platform), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &FilesystemOutputAuthority{authority: runtimeAuthority}
}
