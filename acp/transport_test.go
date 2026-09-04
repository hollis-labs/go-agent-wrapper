package acp

import (
	"errors"
	"sync/atomic"
	"testing"
)

func TestTransportTerminationJoinsMalformedBeforeProcessExit(t *testing.T) {
	termination := NewTransportTermination(true)
	termination.ReportProcess(TransportProcessResult{ExitCode: 9, Err: errors.New("exit status 9")})
	termination.ReportRead(TransportReadResult{Malformed: true})
	result := termination.Coordinate(nil, nil)
	if !result.Read.Malformed || result.Process.ExitCode != 9 || !result.ShouldEmitProcessExit() {
		t.Fatalf("joined termination = %+v", result)
	}
}

func TestTransportTerminationReapsReaderFirstWithoutPromotingConsequence(t *testing.T) {
	termination := NewTransportTermination(true)
	termination.ReportRead(TransportReadResult{})
	var stops atomic.Int32
	var kills atomic.Int32
	resultCh := make(chan TransportTerminationResult, 1)
	go func() {
		resultCh <- termination.Coordinate(func() {
			stops.Add(1)
			termination.ReportProcess(TransportProcessResult{ExitCode: 0})
		}, func() error {
			kills.Add(1)
			return nil
		})
	}()
	result := <-resultCh
	if stops.Load() != 1 || kills.Load() != 0 || !result.Disconnected || result.KilledAfterRead || result.ShouldEmitProcessExit() {
		t.Fatalf("reader-first result=%+v stops=%d kills=%d", result, stops.Load(), kills.Load())
	}
}
