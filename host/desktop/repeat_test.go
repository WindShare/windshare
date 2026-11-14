package desktop

import (
	"context"
	"slices"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestRepeatAuthorizationRetainsSelectionWithoutResurrectingFailedTask(t *testing.T) {
	a, original, starts := fixture(t)
	a.execute(createRequest())
	admitted(t, a)
	original.finish()
	original.mu.Lock()
	original.state.Startup = engine.ShareStartupFailed
	original.state.Result.Outcome = engine.OutcomeFailed
	original.state.Authorization = &engine.ShareAuthorization{Kind: engine.ShareSelfHostedAuthorization, RelayURL: "https://chosen.example"}
	original.mu.Unlock()
	request := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
	reply := a.execute(request)
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	repeated := awaitShare(t, a, request.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })
	if repeated.TaskID != "" || repeated.Authorization.RelayURL != "https://chosen.example" || starts.Load() != 1 {
		t.Fatal("authorization retried a rejected task before setup", repeated)
	}
	a.execute(request)
	if len(a.snapshot().Shares) != 2 {
		t.Fatal("repeat replay duplicated the selection")
	}
	a.mu.Lock()
	if !slices.Equal(a.operations[request.OperationID].paths, a.operations["operation"].paths) {
		t.Error("host did not retain the selected paths")
	}
	a.mu.Unlock()
	var newStarts int
	a.deps.start = func(_ context.Context, request engine.ShareRequest) (shareTask, error) {
		newStarts++
		candidates := request.RelayIntent.Candidates()
		if len(candidates) != 1 || candidates[0].PublicURL != "https://chosen.example" {
			t.Error("authorized selection lost its explicit relay")
		}
		return nil, engine.ErrNetworkUnavailable
	}
	a.deps.setup = func(_ context.Context, input SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
	}
	setup := setupRequest()
	setup.OperationID = request.OperationID
	a.execute(setup)
	a.work.Wait()
	if newStarts != 1 || a.operations["operation"].phase.(running).task.Snapshot().Result.Outcome != engine.OutcomeFailed {
		t.Fatal("setup did not create a fresh task attempt or overwrote the old result")
	}
}

func TestCancelledRepeatCannotAdmitOnReplay(t *testing.T) {
	a, original, starts := fixture(t)
	a.execute(createRequest())
	admitted(t, a)
	original.finish()
	request := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
	a.execute(Request{Command: "cancel", OperationID: request.OperationID})
	if reply := a.execute(request); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	if starts.Load() != 1 {
		t.Fatal("replayed repeat escaped cancellation")
	}
}
