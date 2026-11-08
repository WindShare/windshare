package cli

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

type shareRequest struct {
	sharing     relayconfig.Config
	paths       []string
	relays      []relayprofile.Candidate
	link        shareLinkPresentation
	chunkSize   uint32
	observation observationOptions
}

func (a *App) parseShareRequest(args []string) (shareRequest, requestParseOutcome) {
	flags := a.newFlagSet("share")
	var relayURLs []string
	flags.Func("relay", "override saved relay selection; repeat to use multiple relays", func(value string) error {
		if value == "" {
			return errors.New("relay URL is empty")
		}
		if slices.Contains(relayURLs, value) {
			return nil
		}
		if len(relayURLs) >= relayprofile.MaximumEndpoints {
			return errors.New("too many relay endpoints")
		}
		relayURLs = append(relayURLs, value)
		return nil
	})
	blockSize := flags.Int64("block-size", 0, fmt.Sprintf("file-local block size in bytes; 0 uses the default, otherwise a power of two from %d to %d",
		catalog.MinChunkSize, catalog.MaxChunkSize))
	splitKey := flags.Bool("split-key", false, "print a bare link and separate key string")
	frontURL := flags.String("front-url", "", "override the saved frontend base URL for this share")
	browserTrace := flags.Bool("browser-trace", false, "enable browser diagnostics when the generated link is opened")
	var observation observationOptions
	if err := bindObservationOptions(flags, &observation); err != nil {
		a.writeCompleteLine("share: observation options are unavailable")
		return shareRequest{}, requestParseInternalFailure
	}
	paths, flagParse := parseInterleaved(flags, args)
	if parse := a.projectFlagParse("share", flags, "share <path...> [options]", flagParse); parse != requestParseReady {
		return shareRequest{}, parse
	}
	if err := observation.validate(); err != nil {
		a.writeCompleteLine("share: %s", observationOptionDiagnostic(err))
		return shareRequest{}, requestParseUsageFailure
	}
	if len(paths) == 0 {
		a.writeCompleteLine("share: at least one path is required")
		return shareRequest{}, requestParseUsageFailure
	}
	// Report invalid flags before configuration or workflow failures can obscure
	// the user's input error.
	if *blockSize != 0 && (*blockSize < 0 || *blockSize > math.MaxUint32 || !catalog.ValidChunkSize(uint32(*blockSize))) {
		a.writeCompleteLine("share: --block-size must be 0 for the default or a power of two from %d to %d bytes",
			catalog.MinChunkSize, catalog.MaxChunkSize)
		return shareRequest{}, requestParseUsageFailure
	}
	// The process entry point supplies personal configuration. Embedded callers
	// and tests must not acquire authority from the developer's machine implicitly.
	var saved relayconfig.Config
	if a.loadShareConfig != nil {
		var err error
		saved, err = a.loadShareConfig()
		if err != nil {
			if errors.Is(err, relayconfig.ErrFrontURL) {
				a.writeCompleteLine("share: %s", err)
			} else {
				a.writeCompleteLine("share: cannot load sharing configuration; check windshare/config.json or WINDSHARE_CONFIG")
			}
			return shareRequest{}, requestParseUsageFailure
		}
	}
	// Presence matters: an explicitly empty override must fail instead of silently
	// restoring a saved frontend the caller intended to replace.
	frontOverride := false
	flags.Visit(func(option *flag.Flag) {
		frontOverride = frontOverride || option.Name == "front-url"
	})
	if !frontOverride {
		*frontURL = saved.FrontURL
	}
	if err := relayconfig.ValidateFrontURL(*frontURL); err != nil {
		if *frontURL == "" {
			a.writeCompleteLine("share: configure a frontend URL with wind config import <file|-> --front-url <url>, or pass --front-url for this share")
		} else {
			a.writeCompleteLine("share: %s", err)
		}
		return shareRequest{}, requestParseUsageFailure
	}
	relays, err := relayconfig.SelectRelays(saved, relayURLs)
	if err != nil {
		a.writeCompleteLine("share: configure relay candidates with wind config import <file|->, or pass --relay for this share")
		return shareRequest{}, requestParseUsageFailure
	}
	return shareRequest{
		sharing: saved,
		paths:   paths, relays: relays,
		link:      shareLinkPresentation{frontURL: *frontURL, splitKey: *splitKey, browserTrace: *browserTrace},
		chunkSize: uint32(*blockSize), observation: observation,
	}, requestParseReady
}
