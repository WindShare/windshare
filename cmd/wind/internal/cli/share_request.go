package cli

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"slices"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnconfig"
)

type shareRequest struct {
	turn        turnconfig.Config
	paths       []string
	relays      []senderrelay.Candidate
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
		if len(relayURLs) >= relayset.MaximumEndpoints {
			return errors.New("too many relay endpoints")
		}
		relayURLs = append(relayURLs, value)
		return nil
	})
	blockSize := flags.Int64("block-size", 0, "file-local block size in bytes; 0 uses 1 MiB")
	splitKey := flags.Bool("split-key", false, "print a bare link and separate key string")
	frontURL := flags.String("front-url", "", "override the saved frontend base URL for this share")
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
	if len(paths) == 0 {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: at least one path is required")
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
	// The process entry point supplies personal configuration. Embedded callers
	// and tests must not acquire authority from the developer's machine implicitly.
	var saved relayconfig.Config
	if a.loadShareConfig != nil {
		var err error
		saved, err = a.loadShareConfig()
		if err != nil {
			_, _ = fmt.Fprintln(a.stderrWriter(), "share: cannot load sharing configuration; check windshare/config.json or WINDSHARE_CONFIG")
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
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: configure a frontend URL with wind config import <file|-> --front-url <url>, or pass --front-url for this share; expected an HTTP(S) base URL without credentials, query or fragment")
		return shareRequest{}, requestParseUsageFailure
	}
	relays, err := relayconfig.SelectRelays(saved, relayURLs)
	if err != nil {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: configure relay candidates with wind config import <file|->, or pass --relay for this share")
		return shareRequest{}, requestParseUsageFailure
	}
	return shareRequest{
		turn:  saved.TURN,
		paths: paths, relays: relays,
		link:      shareLinkPresentation{frontURL: *frontURL, splitKey: *splitKey, browserTrace: *browserTrace},
		chunkSize: uint32(chunkSize), observation: observation,
	}, requestParseReady
}
