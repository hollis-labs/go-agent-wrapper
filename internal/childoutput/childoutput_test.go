package childoutput

import (
	"io"
	"testing"
	"time"
)

// The child wrote its last line and exited before the reader got to it (the
// order cmd.Wait returning first produces). Drain must leave the read end
// open until the reader has read that line and reached EOF.
func TestDrainKeepsOutputWrittenBeforeExit(t *testing.T) {
	p, err := NewPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := p.W.Write([]byte("{\"id\":7,\"result\":{}}\n")); werr != nil {
		t.Fatal(werr)
	}
	p.Started() // the child has exited: no write end left

	readerDone := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		Drain(p.R, readerDone, time.Now().Add(DrainTimeout))
		close(drained)
	}()

	got, err := io.ReadAll(p.R)
	close(readerDone)
	if err != nil {
		t.Fatalf("read after exit: %v", err)
	}
	if string(got) != "{\"id\":7,\"result\":{}}\n" {
		t.Fatalf("read %q, want the final frame", got)
	}
	<-drained
}

// A descendant that inherited the write end keeps it open after the child
// exits. Drain closes the read end at the deadline, which unblocks the reader.
func TestDrainUnblocksReaderAtDeadline(t *testing.T) {
	p, err := NewPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.W.Close() }() // the descendant's copy

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, _ = io.ReadAll(p.R)
	}()

	Drain(p.R, readerDone, time.Now().Add(50*time.Millisecond))
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader still blocked after Drain closed the read end")
	}
}

// A reader stalled outside Read (for example, in a handler) never signals
// done. Drain must still return at the deadline so the exit is reported.
func TestDrainReturnsAtDeadlineForStalledReader(t *testing.T) {
	p, err := NewPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.Started()

	returned := make(chan struct{})
	go func() {
		Drain(p.R, make(chan struct{}), time.Now().Add(50*time.Millisecond))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain waited past its deadline for a stalled reader")
	}
}
