package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const validPermissionParams = `{
  "sessionId":"session-1",
  "toolCall":{"toolCallId":"call-1","title":"Run tests","rawInput":{"command":"go test ./...","token":"request-secret"}},
  "options":[
    {"optionId":"allow-1","name":"Allow once","kind":"allow_once"},
    {"optionId":"deny-1","name":"Reject once","kind":"reject_once"}
  ],
  "_meta":{"providerExtension":true}
}`

func TestBestEffortPermissionRequestsDefaultAndSelections(t *testing.T) {
	tests := []struct {
		name       string
		responder  BestEffortPermissionRequestResponder
		outcome    string
		optionID   string
		allowed    bool
		diagnostic bool
	}{
		{name: "default", outcome: "cancelled"},
		{
			name: "allow",
			responder: func(_ context.Context, request PermissionRequest) (PermissionSelection, error) {
				assertPermissionRequest(t, request)
				return SelectPermissionOption("allow-1"), nil
			},
			outcome: "selected", optionID: "allow-1", allowed: true,
		},
		{
			name: "deny is selected but not allowed",
			responder: func(_ context.Context, _ PermissionRequest) (PermissionSelection, error) {
				return SelectPermissionOption("deny-1"), nil
			},
			outcome: "selected", optionID: "deny-1", allowed: false,
		},
		{
			name: "explicit cancel",
			responder: func(_ context.Context, _ PermissionRequest) (PermissionSelection, error) {
				return PermissionSelection{}, nil
			},
			outcome: "cancelled",
		},
		{
			name: "unoffered option",
			responder: func(_ context.Context, _ PermissionRequest) (PermissionSelection, error) {
				return SelectPermissionOption("not-offered"), nil
			},
			outcome: "cancelled", diagnostic: true,
		},
		{
			name: "responder cannot mutate offered options",
			responder: func(_ context.Context, request PermissionRequest) (PermissionSelection, error) {
				request.Options[0].OptionID = "mutated"
				return SelectPermissionOption("mutated"), nil
			},
			outcome: "cancelled", diagnostic: true,
		},
		{
			name: "responder error is not echoed",
			responder: func(_ context.Context, _ PermissionRequest) (PermissionSelection, error) {
				return PermissionSelection{}, errors.New("password=callback-secret")
			},
			outcome: "cancelled", diagnostic: true,
		},
		{
			name: "responder panic",
			responder: func(_ context.Context, _ PermissionRequest) (PermissionSelection, error) {
				panic("token=panic-secret")
			},
			outcome: "cancelled", diagnostic: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := NewBestEffortPermissionRequests(test.responder)
			requests.BeginTurn()
			var wire map[string]any
			resolution := requests.Respond(json.RawMessage(validPermissionParams), func(got PermissionResolution) error {
				wire = got.Result()
				return nil
			})
			if string(resolution.Outcome) != test.outcome || resolution.Option.OptionID != test.optionID || resolution.Allowed() != test.allowed {
				t.Fatalf("resolution = %+v allowed=%v, want outcome=%q option=%q allowed=%v", resolution, resolution.Allowed(), test.outcome, test.optionID, test.allowed)
			}
			encoded, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"outcome":"`+test.outcome+`"`) {
				t.Fatalf("wire result = %s, want outcome %q", encoded, test.outcome)
			}
			if test.optionID != "" && !strings.Contains(string(encoded), `"optionId":"`+test.optionID+`"`) {
				t.Fatalf("wire result = %s, want option %q", encoded, test.optionID)
			}
			diagnostic, ok := resolution.Diagnostic()
			if ok != test.diagnostic {
				t.Fatalf("Diagnostic present = %v, want %v (%+v)", ok, test.diagnostic, diagnostic)
			}
			if strings.Contains(diagnostic.Message+diagnostic.Raw, "callback-secret") || strings.Contains(diagnostic.Message+diagnostic.Raw, "panic-secret") || strings.Contains(diagnostic.Message+diagnostic.Raw, "request-secret") {
				t.Fatalf("diagnostic leaked secret: %+v", diagnostic)
			}
		})
	}
}

func TestBestEffortPermissionRequestsInvalidParamsFailClosed(t *testing.T) {
	tests := []string{
		`{"sessionId":"","toolCall":{"toolCallId":"call"},"options":[]}`,
		`{"sessionId":"session","toolCall":{"toolCallId":""},"options":[]}`,
		`{"sessionId":"session","toolCall":{"toolCallId":"call"}}`,
		`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":null}`,
		`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"","name":"x","kind":"allow_once"}]}`,
		`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"x","name":"","kind":"allow_once"}]}`,
		`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"x","name":"x","kind":""}]}`,
		`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"same","name":"x","kind":"allow_once"},{"optionId":"same","name":"y","kind":"reject_once"}],"password":"raw-secret"}`,
		`{"not":"complete"`,
	}
	for _, raw := range tests {
		requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
			t.Fatal("responder called for invalid request")
			return PermissionSelection{}, nil
		})
		requests.BeginTurn()
		resolution := requests.Respond(json.RawMessage(raw), func(PermissionResolution) error { return nil })
		if resolution.Outcome != "cancelled" {
			t.Fatalf("invalid request outcome = %q, want cancelled", resolution.Outcome)
		}
		diagnostic, ok := resolution.Diagnostic()
		if !ok || strings.Contains(diagnostic.Message+diagnostic.Raw, "raw-secret") {
			t.Fatalf("invalid request diagnostic missing or leaked: %+v", diagnostic)
		}
	}
}

func TestBestEffortPermissionRequestsSessionMismatchFailsClosed(t *testing.T) {
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		t.Fatal("responder called for a different session")
		return PermissionSelection{}, nil
	})
	requests.SetSessionID("another-session")
	requests.BeginTurn()
	resolution := requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	if resolution.Outcome != "cancelled" || resolution.Reason != "session mismatch" {
		t.Fatalf("session mismatch resolution = %+v", resolution)
	}
	if _, ok := resolution.Diagnostic(); !ok {
		t.Fatal("session mismatch omitted safe diagnostic")
	}
}

func TestBestEffortPermissionRequestsCancelGenerationAndRecoverNextTurn(t *testing.T) {
	entered := make(chan struct{}, 1)
	requests := NewBestEffortPermissionRequests(func(ctx context.Context, _ PermissionRequest) (PermissionSelection, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return SelectPermissionOption("allow-1"), ctx.Err()
	})
	requests.BeginTurn()
	resolutionCh := make(chan PermissionResolution, 1)
	go func() {
		resolutionCh <- requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("responder was not entered")
	}
	requests.CancelTurn()
	select {
	case resolution := <-resolutionCh:
		if resolution.Outcome != "cancelled" {
			t.Fatalf("cancelled turn outcome = %q, want cancelled", resolution.Outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("CancelTurn did not unblock responder coordination")
	}

	// A request arriving after cancellation must not invoke the responder.
	late := requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	if late.Outcome != "cancelled" || late.Reason != "turn not active" {
		t.Fatalf("late cancelled-turn resolution = %+v", late)
	}
	select {
	case <-entered:
		t.Fatal("late request invoked responder after turn cancellation")
	default:
	}

	requests.responder = func(_ context.Context, _ PermissionRequest) (PermissionSelection, error) {
		return SelectPermissionOption("allow-1"), nil
	}
	requests.BeginTurn()
	next := requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	if next.Outcome != "selected" || !next.Allowed() {
		t.Fatalf("next-turn resolution = %+v, want selected allow", next)
	}
}

func TestBestEffortPermissionRequestsConcurrentCorrelation(t *testing.T) {
	requests := NewBestEffortPermissionRequests(func(_ context.Context, request PermissionRequest) (PermissionSelection, error) {
		if request.ToolCall.ToolCallID == "allow-call" {
			return SelectPermissionOption("allow"), nil
		}
		return SelectPermissionOption("deny"), nil
	})
	requests.BeginTurn()

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wantAllow := i%2 == 0
		callID, optionID, kind := "deny-call", "deny", "reject_always"
		if wantAllow {
			callID, optionID, kind = "allow-call", "allow", "allow_always"
		}
		raw := json.RawMessage(`{"sessionId":"session","toolCall":{"toolCallId":"` + callID + `"},"options":[{"optionId":"` + optionID + `","name":"choice","kind":"` + kind + `"}]}`)
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolution := requests.Respond(raw, func(PermissionResolution) error { return nil })
			if resolution.Outcome != "selected" || resolution.Allowed() != wantAllow || resolution.Option.OptionID != optionID {
				t.Errorf("resolution = %+v allowed=%v, want option=%q allowed=%v", resolution, resolution.Allowed(), optionID, wantAllow)
			}
		}()
	}
	wg.Wait()
}

func TestBestEffortPermissionRequestsCloseDoesNotWaitForIgnoringCallback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		close(entered)
		<-release // Deliberately ignore context until the test releases us.
		return PermissionSelection{}, nil
	})
	requests.BeginTurn()
	done := make(chan PermissionResolution, 1)
	go func() {
		done <- requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	}()
	<-entered
	requests.Close()
	select {
	case resolution := <-done:
		if resolution.Outcome != "cancelled" {
			t.Fatalf("Close resolution = %+v", resolution)
		}
		close(release)
	case <-time.After(time.Second):
		t.Fatal("Close waited for a callback that ignored context")
	}
}

func TestBestEffortPermissionRequestsLifecycleReentryFromResponseDoesNotDeadlock(t *testing.T) {
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		return SelectPermissionOption("allow-1"), nil
	})
	requests.BeginTurn()
	done := make(chan PermissionResolution, 1)
	go func() {
		done <- requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error {
			requests.CancelTurn()
			requests.Close()
			return nil
		})
	}()
	select {
	case resolution := <-done:
		if resolution.Outcome != PermissionOutcomeSelected {
			t.Fatalf("committed resolution = %+v, want selected", resolution)
		}
	case <-time.After(time.Second):
		t.Fatal("response callback deadlocked while re-entering lifecycle methods")
	}
}

func TestBestEffortPermissionRequestsResponseErrorIsRedactedAndFailsEventClosed(t *testing.T) {
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		return SelectPermissionOption("allow-1"), nil
	})
	requests.BeginTurn()
	resolution := requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error {
		return errors.New("token=response-write-secret")
	})
	if resolution.ResponseError() == nil {
		t.Fatal("response delivery error was discarded")
	}
	diagnostic, ok := resolution.Diagnostic()
	if !ok || strings.Contains(diagnostic.Message+diagnostic.Raw, "response-write-secret") {
		t.Fatalf("response error diagnostic missing or leaked: %+v", diagnostic)
	}
	payload := resolution.ResolvedEventPayload("request")
	if payload["allowed"] != false || payload["outcome"] != PermissionOutcomeCancelled || payload["reason"] != "permission response delivery failed" {
		t.Fatalf("response error event payload = %+v", payload)
	}
}

func TestBestEffortPermissionRequestsEndTurnWaitsForDispatchedResponse(t *testing.T) {
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		return SelectPermissionOption("allow-1"), nil
	})
	requests.BeginTurn()
	responseEntered := make(chan struct{})
	releaseResponse := make(chan struct{})
	requests.Dispatch(json.RawMessage(validPermissionParams), func(PermissionResolution) error {
		close(responseEntered)
		<-releaseResponse
		return nil
	}, nil)
	<-responseEntered
	endDone := make(chan struct{})
	go func() {
		requests.EndTurn()
		close(endDone)
	}()
	select {
	case <-endDone:
		t.Fatal("EndTurn crossed a dispatched permission response")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseResponse)
	select {
	case <-endDone:
	case <-time.After(time.Second):
		t.Fatal("EndTurn did not resume after dispatched response finished")
	}
}

func TestBestEffortPermissionRequestsCloseDoesNotWaitForBlockedResponse(t *testing.T) {
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		return SelectPermissionOption("allow-1"), nil
	})
	requests.BeginTurn()
	responseEntered := make(chan struct{})
	releaseResponse := make(chan struct{})
	respondDone := make(chan struct{})
	go func() {
		defer close(respondDone)
		requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error {
			close(responseEntered)
			<-releaseResponse // Model a backpressured pipe or socket write.
			return nil
		})
	}()
	<-responseEntered
	closeDone := make(chan struct{})
	go func() {
		requests.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close waited for blocked response I/O")
	}
	close(releaseResponse)
	select {
	case <-respondDone:
	case <-time.After(time.Second):
		t.Fatal("response did not finish after backpressure was released")
	}
}

func TestBestEffortPermissionRequestsBoundsIgnoringCallbacksAcrossTurns(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, MaxConcurrentBestEffortPermissionRequests+1)
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		entered <- struct{}{}
		<-release // Deliberately ignore cancellation.
		return PermissionSelection{}, nil
	})
	requests.BeginTurn()

	const overflow = 8
	results := make(chan PermissionResolution, MaxConcurrentBestEffortPermissionRequests+overflow+1)
	for i := 0; i < MaxConcurrentBestEffortPermissionRequests+overflow; i++ {
		go func() {
			results <- requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
		}()
	}
	for i := 0; i < MaxConcurrentBestEffortPermissionRequests; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatalf("responder callback %d was not admitted", i+1)
		}
	}
	select {
	case <-entered:
		t.Fatal("responder callback admission exceeded the documented bound")
	case <-time.After(50 * time.Millisecond):
	}

	requests.CancelTurn()
	for i := 0; i < MaxConcurrentBestEffortPermissionRequests+overflow; i++ {
		select {
		case resolution := <-results:
			if resolution.Outcome != PermissionOutcomeCancelled {
				t.Errorf("bounded request resolution = %+v", resolution)
			}
		case <-time.After(time.Second):
			t.Fatalf("request %d did not complete after cancellation", i+1)
		}
	}

	requests.BeginTurn()
	capacityResolution := requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	if capacityResolution.Outcome != PermissionOutcomeCancelled || capacityResolution.Reason != "responder capacity reached" {
		t.Fatalf("capacity retained across cancellation = %+v, want bounded cancellation", capacityResolution)
	}
	if diagnostic, ok := capacityResolution.Diagnostic(); !ok || strings.Contains(diagnostic.Message+diagnostic.Raw, "request-secret") {
		t.Fatalf("capacity diagnostic missing or leaked: %+v", diagnostic)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		requests.mu.Lock()
		callbacks := requests.callbacks
		requests.mu.Unlock()
		if callbacks == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d responder callbacks remained after release", callbacks)
		}
		time.Sleep(time.Millisecond)
	}
	requests.BeginTurn()
	recovered := requests.Respond(json.RawMessage(validPermissionParams), func(PermissionResolution) error { return nil })
	if recovered.Outcome != PermissionOutcomeCancelled || recovered.Reason != "responder cancelled" {
		t.Fatalf("responder capacity did not recover after callbacks exited: %+v", recovered)
	}
}

func TestBestEffortPermissionRequestsAdmissionGenerationSurvivesEndAndBeginFlood(t *testing.T) {
	var responderCalls atomic.Int64
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		responderCalls.Add(1)
		return SelectPermissionOption("allow-1"), nil
	})
	requests.BeginTurn()

	const overflow = 16
	requestCount := MaxConcurrentBestEffortPermissionRequests + overflow
	admitted := make(chan PermissionDispatchAdmission, requestCount)
	releaseAdmission := make(chan struct{})
	responses := make(chan struct {
		admission  PermissionDispatchAdmission
		resolution PermissionResolution
	}, requestCount)
	for i := 0; i < requestCount; i++ {
		go requests.DispatchTurnRequest(json.RawMessage(validPermissionParams), func(admission PermissionDispatchAdmission) {
			admitted <- admission
			<-releaseAdmission
		}, func(admission PermissionDispatchAdmission, resolution PermissionResolution) error {
			responses <- struct {
				admission  PermissionDispatchAdmission
				resolution PermissionResolution
			}{admission: admission, resolution: resolution}
			return nil
		}, nil)
	}

	for i := 0; i < requestCount; i++ {
		select {
		case admission := <-admitted:
			if !admission.ActiveTurn || admission.Generation != 1 {
				t.Fatalf("admission %d = %+v, want active generation 1", i, admission)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/%d requests reached the admission barrier", i, requestCount)
		}
	}

	endDone := make(chan struct{})
	go func() {
		requests.EndTurn()
		close(endDone)
	}()
	waitForPermissionTurnEnding(t, requests, 1)
	select {
	case <-endDone:
		t.Fatal("EndTurn returned before admitted requests completed")
	default:
	}

	// A later generation may begin while old dispatch workers are queued. The
	// immutable admission still forces every generation-1 request to cancel.
	requests.BeginTurn()
	close(releaseAdmission)
	for i := 0; i < requestCount; i++ {
		select {
		case response := <-responses:
			if response.admission.Generation != 1 || !response.admission.ActiveTurn {
				t.Errorf("response admission = %+v", response.admission)
			}
			if response.resolution.Outcome != PermissionOutcomeCancelled {
				t.Errorf("old-generation resolution = %+v, want cancelled", response.resolution)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/%d old-generation responses completed", i, requestCount)
		}
	}
	select {
	case <-endDone:
	case <-time.After(time.Second):
		t.Fatal("EndTurn did not release after every admitted response completed")
	}
	if got := responderCalls.Load(); got != 0 {
		t.Fatalf("old-generation requests invoked responder %d times", got)
	}

	newResponse := make(chan PermissionResolution, 1)
	requests.DispatchTurnRequest(json.RawMessage(validPermissionParams), nil, func(admission PermissionDispatchAdmission, resolution PermissionResolution) error {
		if !admission.ActiveTurn || admission.Generation != 2 {
			t.Errorf("new-turn admission = %+v", admission)
		}
		newResponse <- resolution
		return nil
	}, nil)
	select {
	case resolution := <-newResponse:
		if resolution.Outcome != PermissionOutcomeSelected || resolution.Option.OptionID != "allow-1" {
			t.Fatalf("new-turn resolution = %+v", resolution)
		}
	case <-time.After(time.Second):
		t.Fatal("new-turn permission did not complete")
	}
	if got := responderCalls.Load(); got != 1 {
		t.Fatalf("responder calls after new turn = %d, want 1", got)
	}
	requests.EndTurn()
}

func TestBestEffortPermissionRequestsLateClosedAdmissionCannotBecomeNextTurn(t *testing.T) {
	var responderCalls atomic.Int64
	requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
		responderCalls.Add(1)
		return SelectPermissionOption("allow-1"), nil
	})
	requests.BeginTurn()
	requests.EndTurn()

	respondEntered := make(chan PermissionDispatchAdmission, 1)
	releaseResponse := make(chan struct{})
	completed := make(chan PermissionResolution, 1)
	go requests.DispatchTurnRequest(json.RawMessage(validPermissionParams), func(admission PermissionDispatchAdmission) {
		if admission.ActiveTurn {
			t.Errorf("late request admitted as active: %+v", admission)
		}
	}, func(admission PermissionDispatchAdmission, resolution PermissionResolution) error {
		respondEntered <- admission
		<-releaseResponse
		completed <- resolution
		return nil
	}, nil)

	var lateAdmission PermissionDispatchAdmission
	select {
	case lateAdmission = <-respondEntered:
	case <-time.After(time.Second):
		t.Fatal("late permission response did not reach transport")
	}
	if lateAdmission.ActiveTurn || lateAdmission.Generation != 1 {
		t.Fatalf("late admission = %+v, want inactive generation 1", lateAdmission)
	}
	requests.BeginTurn()
	close(releaseResponse)
	select {
	case resolution := <-completed:
		if resolution.Outcome != PermissionOutcomeCancelled || resolution.Reason != "turn not active" {
			t.Fatalf("late resolution = %+v, want inactive cancellation", resolution)
		}
	case <-time.After(time.Second):
		t.Fatal("late permission response did not complete")
	}
	if got := responderCalls.Load(); got != 0 {
		t.Fatalf("late request invoked next-turn responder %d times", got)
	}
	requests.EndTurn()
}

func waitForPermissionTurnEnding(t *testing.T, requests *BestEffortPermissionRequests, generation uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		requests.mu.Lock()
		turn := requests.turns[generation]
		ending := turn != nil && turn.ending
		requests.mu.Unlock()
		if ending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission generation %d did not start ending", generation)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertPermissionRequest(t *testing.T, request PermissionRequest) {
	t.Helper()
	if request.SessionID != "session-1" || request.ToolCall.ToolCallID != "call-1" || len(request.Options) != 2 {
		t.Errorf("request = %+v", request)
	}
	if !strings.Contains(string(request.ToolCall.RawInput), "request-secret") || !strings.Contains(string(request.RawParams), "providerExtension") {
		t.Errorf("request lost raw approval context: %+v", request)
	}
}
