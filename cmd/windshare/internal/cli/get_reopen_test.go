//go:build windows || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type fixedGetShapeResolver struct {
	decision ordinaryoutput.ShapeDecision
	err      error
	calls    int
	events   *[]string
}

func (resolver *fixedGetShapeResolver) ResolveOrdinaryOutputShape(
	context.Context,
	transfer.SelectionSpec,
	ordinaryoutput.ShapeProbeBudget,
	ordinaryoutput.ShapeTracer,
) (ordinaryoutput.ShapeDecision, error) {
	resolver.calls++
	if resolver.events != nil {
		*resolver.events = append(*resolver.events, "shape")
	}
	return resolver.decision, resolver.err
}

type layoutRecordingGetOutputAuthority struct {
	getOutputAuthority
	selection transfer.SelectionSpec
	events    *[]string
	artifact  receivecontract.ArtifactSpec
}

type fixedLookupGetOutputAuthority struct {
	getOutputAuthority
	lookup getOutputLookup
}

func (authority fixedLookupGetOutputAuthority) LookupActive(
	context.Context,
	transfer.SelectionSpec,
) (getOutputLookup, error) {
	return authority.lookup, nil
}

func (authority *layoutRecordingGetOutputAuthority) LookupActive(
	context.Context,
	transfer.SelectionSpec,
) (getOutputLookup, error) {
	*authority.events = append(*authority.events, "lookup")
	return getOutputLookup{kind: getOutputLookupMiss}, nil
}

func (authority *layoutRecordingGetOutputAuthority) CreateOperation(
	_ context.Context,
	_ getOutputLookup,
	artifact receivecontract.ArtifactSpec,
) (getOutputOperation, error) {
	*authority.events = append(*authority.events, "create")
	authority.artifact = artifact
	tree, ok := artifact.DirectoryTree()
	if !ok {
		return getOutputOperation{}, errGetOutputReservationContract
	}
	var requestedName string
	fileLike := false
	switch tree.Kind() {
	case receivecontract.DirectoryTreeSingleFile:
		single, _ := tree.SingleFile()
		requestedName, fileLike = single.SuggestedName, true
	case receivecontract.DirectoryTreeResultRoot:
		root, _ := tree.ResultRoot()
		requestedName = root.Name()
	default:
		return getOutputOperation{}, errGetOutputReservationContract
	}
	var operationID receivecontract.OperationID
	operationID[0] = 1
	var reservationID receivecontract.DestinationReservationID
	reservationID[0] = 2
	var authorityRef receivecontract.AuthorityRef
	authorityRef[0] = 3
	reservedName, err := receivecontract.CollisionName(operationID, requestedName, 0, fileLike)
	if err != nil {
		return getOutputOperation{}, err
	}
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operationID, reservationID, artifact, authorityRef, reservedName, 0,
	)
	if err != nil {
		return getOutputOperation{}, err
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		return getOutputOperation{}, err
	}
	intent, err := transfer.NewReceiveIntent(authority.selection, artifact, plan)
	if err != nil {
		return getOutputOperation{}, err
	}
	return getOutputOperation{intent: intent, mode: getOutputResumable}, nil
}

func TestResolveGetOutputOperationReopensBeforeShapeAndRefusesConcurrentLease(t *testing.T) {
	ctx := context.Background()
	rootPath := newCLICertifiedOutputTestRoot(t)
	openAuthority := func() getOutputAuthority {
		authority, err := newFilesystemGetOutputAuthority(getOutputAuthorityConfig{
			rootPath: rootPath, createRoot: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		mode, err := authority.BindDestination(ctx)
		if err != nil {
			_ = authority.Close()
			t.Fatal(err)
		}
		if mode != getOutputResumable {
			_ = authority.Close()
			t.Fatalf("certified output mode=%d", mode)
		}
		return authority
	}
	selection := getReopenSelection(t, true, nil)
	decision, err := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
	if err != nil {
		t.Fatal(err)
	}

	firstAuthority := openAuthority()
	firstResolver := &fixedGetShapeResolver{decision: decision}
	first, err := resolveGetOutputOperation(ctx, firstAuthority, firstResolver, selection)
	if err != nil {
		t.Fatal(err)
	}
	firstDestination, firstAdjusted, err := getOperationDestination(rootPath, first.operation)
	if err != nil {
		t.Fatal(err)
	}
	if first.lookup != getOutputLookupMiss || firstResolver.calls != 1 || firstAdjusted || firstDestination == "" {
		t.Fatalf("first admission=%+v shape_calls=%d", first, firstResolver.calls)
	}
	if err := firstAuthority.Close(); err != nil {
		t.Fatal(err)
	}

	activeAuthority := openAuthority()
	reopenResolver := &fixedGetShapeResolver{err: errors.New("active reopen must not resolve shape")}
	reopened, err := resolveGetOutputOperation(ctx, activeAuthority, reopenResolver, selection)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.lookup != getOutputLookupReopened || reopenResolver.calls != 0 ||
		!reopened.operation.intent.EqualCanonical(first.operation.intent) {
		t.Fatalf("reopened admission=%+v shape_calls=%d", reopened, reopenResolver.calls)
	}

	contendingAuthority := openAuthority()
	contendingResolver := &fixedGetShapeResolver{err: errors.New("lease contention must not resolve shape")}
	_, err = resolveGetOutputOperation(ctx, contendingAuthority, contendingResolver, selection)
	if !errors.Is(err, errGetOutputOperationAlreadyRunning) || contendingResolver.calls != 0 {
		t.Fatalf("contending error=%v shape_calls=%d", err, contendingResolver.calls)
	}
	if err := contendingAuthority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := activeAuthority.Close(); err != nil {
		t.Fatal(err)
	}

	differentSelection := getReopenSelection(t, false, []string{"different.txt"})
	differentAuthority := openAuthority()
	differentResolver := &fixedGetShapeResolver{decision: decision}
	different, err := resolveGetOutputOperation(
		ctx, differentAuthority, differentResolver, differentSelection,
	)
	if err != nil {
		t.Fatal(err)
	}
	differentDestination, differentAdjusted, err := getOperationDestination(rootPath, different.operation)
	if err != nil {
		t.Fatal(err)
	}
	if different.lookup != getOutputLookupMiss || differentResolver.calls != 1 ||
		different.operation.intent.OperationID() == first.operation.intent.OperationID() ||
		!differentAdjusted || differentDestination == firstDestination {
		t.Fatalf("different admission=%+v shape_calls=%d", different, differentResolver.calls)
	}
	if err := differentAuthority.Close(); err != nil {
		t.Fatal(err)
	}

	finalAuthority := openAuthority()
	finalResolver := &fixedGetShapeResolver{err: errors.New("final reopen must not resolve shape")}
	final, err := resolveGetOutputOperation(ctx, finalAuthority, finalResolver, selection)
	if err != nil {
		t.Fatal(err)
	}
	if final.lookup != getOutputLookupReopened || finalResolver.calls != 0 ||
		final.operation.intent.OperationID() != first.operation.intent.OperationID() {
		t.Fatalf("final admission=%+v shape_calls=%d", final, finalResolver.calls)
	}
	if err := finalAuthority.Close(); err != nil {
		t.Fatal(err)
	}

	firstJob, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	if firstJob == secondJob || bytes.Equal(first.operation.intent.OperationID().Bytes(), firstJob.Bytes()) ||
		bytes.Equal(first.operation.intent.OperationID().Bytes(), secondJob.Bytes()) {
		t.Fatal("stable operation identity was reused as per-run transfer job identity")
	}
}

func getReopenSelection(t *testing.T, wholeShare bool, paths []string) transfer.SelectionSpec {
	t.Helper()
	var share catalog.ShareInstance
	share[0] = 1
	var root catalog.DirectoryID
	root[0] = 2
	var (
		rules transfer.SelectionRules
		err   error
	)
	if wholeShare {
		rules, err = transfer.NewSelectionRules(true, nil)
	} else {
		rules, err = transfer.NewPathSelectionRules(paths)
	}
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func newCLICertifiedOutputTestRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	testBase := filepath.Join(home, ".windshare-test-temp")
	if err := os.MkdirAll(testBase, 0o700); err != nil {
		t.Fatal(err)
	}
	reserved, err := os.MkdirTemp(testBase, ".windshare-cli-reopen-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(reserved); err != nil {
			t.Errorf("remove certified CLI output test root: %v", err)
		}
	})
	return reserved
}

func TestResolveGetOutputOperationRejectsMissingAuthorityOrSelection(t *testing.T) {
	resolver := &fixedGetShapeResolver{}
	if _, err := resolveGetOutputOperation(
		context.Background(), nil, resolver, transfer.SelectionSpec{},
	); !errors.Is(err, errGetOutputReservationContract) {
		t.Fatalf("error=%v", err)
	}
}

func TestResolveGetOutputOperationStopsOnOwnedLookupStateBeforeShape(t *testing.T) {
	selection := getReopenSelection(t, true, nil)
	tests := []struct {
		name string
		kind getOutputLookupKind
		want error
	}{
		{name: "lease already running", kind: getOutputLookupAlreadyRunning, want: errGetOutputOperationAlreadyRunning},
		{name: "operation needs attention", kind: getOutputLookupNeedsAttention, want: errGetOutputOperationNeedsAttention},
		{name: "multiple matches are ambiguous", kind: getOutputLookupAmbiguous, want: errGetOutputOperationAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fixedGetShapeResolver{err: errors.New("lookup state must stop shape resolution")}
			_, err := resolveGetOutputOperation(
				context.Background(),
				fixedLookupGetOutputAuthority{lookup: getOutputLookup{kind: test.kind}},
				resolver,
				selection,
			)
			if !errors.Is(err, test.want) || resolver.calls != 0 {
				t.Fatalf("error=%v shape_calls=%d", err, resolver.calls)
			}
		})
	}
}

func TestResolveGetOutputOperationComposesAllOrdinaryLayouts(t *testing.T) {
	selection := getReopenSelection(t, true, nil)
	var file catalog.FileID
	file[0] = 3
	var directory catalog.DirectoryID
	directory[0] = 4
	single, err := ordinaryoutput.NewSingleFileShape(file, "docs/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	complete, err := ordinaryoutput.NewCompleteDirectoryShape(directory, "photos")
	if err != nil {
		t.Fatal(err)
	}
	partial, err := ordinaryoutput.NewPartialDirectoryShape(directory, "photos")
	if err != nil {
		t.Fatal(err)
	}
	synthetic, err := ordinaryoutput.NewSyntheticSelectionShape(ordinaryoutput.ShapeFallbackMultipleRoots)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		decision   ordinaryoutput.ShapeDecision
		layoutKind receivecontract.DirectoryTreeLayoutKind
		rootClass  receivecontract.ResultRootClass
		outputName string
		sourcePath string
	}{
		{name: "single file", decision: single, layoutKind: receivecontract.DirectoryTreeSingleFile, outputName: "report.txt", sourcePath: "docs/report.txt"},
		{name: "complete directory", decision: complete, layoutKind: receivecontract.DirectoryTreeResultRoot, rootClass: receivecontract.ResultRootCompleteDirectory, outputName: "photos", sourcePath: "photos"},
		{name: "partial directory", decision: partial, layoutKind: receivecontract.DirectoryTreeResultRoot, rootClass: receivecontract.ResultRootDirectorySelection, outputName: "photos-selection", sourcePath: "photos"},
		{name: "synthetic selection", decision: synthetic, layoutKind: receivecontract.DirectoryTreeResultRoot, rootClass: receivecontract.ResultRootSyntheticSelection, outputName: "windshare"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := make([]string, 0, 3)
			authority := &layoutRecordingGetOutputAuthority{selection: selection, events: &events}
			resolver := &fixedGetShapeResolver{decision: test.decision, events: &events}
			_, err := resolveGetOutputOperation(
				context.Background(), authority, resolver, selection,
			)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(events, ",") != "lookup,shape,create" {
				t.Fatalf("admission events=%v", events)
			}
			tree, ok := authority.artifact.DirectoryTree()
			if !ok || tree.Kind() != test.layoutKind {
				t.Fatalf("directory tree kind=%d present=%v", tree.Kind(), ok)
			}
			if test.layoutKind == receivecontract.DirectoryTreeSingleFile {
				singleFile, ok := tree.SingleFile()
				if !ok || singleFile.SuggestedName != test.outputName || singleFile.SourcePath != test.sourcePath {
					t.Fatalf("single-file layout=%+v present=%v", singleFile, ok)
				}
				return
			}
			root, ok := tree.ResultRoot()
			if !ok || root.Class() != test.rootClass || root.Name() != test.outputName || root.SourcePath() != test.sourcePath {
				t.Fatalf("result-root layout class=%d name=%q source=%q present=%v", root.Class(), root.Name(), root.SourcePath(), ok)
			}
		})
	}
}

func TestPrepareGetOutputCreatesMissingContainerFromExistingParent(t *testing.T) {
	parent := newCLICertifiedOutputTestRoot(t)
	container := filepath.Join(parent, "downloads")
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, code := app.prepareGetOutput(
		context.Background(), getRequest{outDir: container}, getObservation{runtime: runtime},
	)
	if code != ExitOK {
		t.Fatalf("prepare exit=%d stderr=%q", code, stderr.String())
	}
	defer func() {
		if err := prepared.authority.Close(); err != nil {
			t.Errorf("close output authority: %v", err)
		}
	}()
	info, err := os.Stat(container)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || prepared.mode != getOutputResumable {
		t.Fatalf("container=%q mode=%d", container, prepared.mode)
	}
	runtime.Close()
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("bind wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
