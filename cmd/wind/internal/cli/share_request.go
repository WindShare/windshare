package cli

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
)

type shareRequest struct {
	paths       []string
	relayURLs   []string
	link        shareLinkPresentation
	chunkSize   uint32
	observation observationOptions
}

func (a *App) parseShareRequest(args []string) (shareRequest, requestParseOutcome) {
	flags := a.newFlagSet("share")
	var relayURLs []string
	flags.Func("relay", "relay server base URL; repeat to use multiple relays", func(value string) error {
		if value == "" {
			return errors.New("relay URL is empty")
		}
		if slices.Contains(relayURLs, value) {
			return nil
		}
		if len(relayURLs) >= relayset.MaximumEndpoints {
			return errors.New("too many relay endpoints")
		}
		relayURLs = append(relayURLs, value)
		return nil
	})
	blockSize := flags.Int64("block-size", 0, "file-local block size in bytes; 0 uses 1 MiB")
	splitKey := flags.Bool("split-key", false, "print a bare link and separate key string")
	frontURL := flags.String("front-url", DefaultFrontURL, "frontend base URL embedded in the link")
	browserTrace := flags.Bool("browser-trace", false, "enable browser diagnostics when the generated link is opened")
	var observation observationOptions
	if err := bindObservationOptions(flags, &observation); err != nil {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: observation options are unavailable")
		return shareRequest{}, requestParseInternalFailure
	}
	paths, flagParse := parseInterleaved(flags, args)
	if parse := a.projectFlagParse("share", flags, "share <path...> [options]", flagParse); parse != requestParseReady {
		return shareRequest{}, parse
	}
	if err := observation.validate(); err != nil {
		_, _ = fmt.Fprintf(a.stderrWriter(), "share: %s\n", observationOptionDiagnostic(err))
		return shareRequest{}, requestParseUsageFailure
	}
	if len(relayURLs) == 0 {
		relayURLs = []string{DefaultRelayURL}
	}
	if len(paths) == 0 || *frontURL == "" {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: at least one path, a relay URL, and a frontend URL are required")
		return shareRequest{}, requestParseUsageFailure
	}
	chunkSize := int64(catalog.DefaultChunkSize)
	if *blockSize != 0 {
		chunkSize = *blockSize
	}
	if chunkSize < 0 || chunkSize > math.MaxUint32 {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: block size is outside the suite-02 range")
		return shareRequest{}, requestParseUsageFailure
	}
	return shareRequest{
		paths: paths, relayURLs: relayURLs,
		link:      shareLinkPresentation{frontURL: *frontURL, splitKey: *splitKey, browserTrace: *browserTrace},
		chunkSize: uint32(chunkSize), observation: observation,
	}, requestParseReady
}
