package wrapper

import (
	"bytes"
	"context"
	"sync"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/filters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// streamWriter is an io.Writer that surfaces a child-process byte
// stream (stdout or stderr) onto the wrapper's activity stream. Each
// Write emits one rawKind event with the full byte chunk and zero or
// more lineKind events as newlines are observed. Partial trailing
// lines are buffered until the next Write completes them.
//
// Note on fidelity per runtime: agentkit's
// [agentsessions.StartOptions.Fanout] surface — which we wire here
// for stdout — is "session output" rather than "child stdout bytes"
// for the non-PTY runtimes. The PTY runtime tees true PTY bytes;
// the adapter and streaming-stdio runtimes write parsed-and-formatted
// per-turn output (e.g. delta content concatenated, followed by a
// "[turn_done]" marker on terminal events). For high-fidelity raw
// replay use PTY runtimes; for the others these events are still a
// useful audit trail, just not byte-exact.
//
// Safe for concurrent Write calls — the agentkit reader and any
// caller-issued writes serialize via the internal mutex.
//
// streamWriter is intentionally not a public API; it's an internal
// adapter the wrapper wires into agentkit's [agentsessions.StartOptions.Fanout]
// and [agentsessions.StartOptions.Stderr] surfaces.
type streamWriter struct {
	bridge   *activity.Bridge
	ctx      context.Context
	source   runtimeevents.Source
	rawKind  runtimeevents.EventKind
	lineKind runtimeevents.EventKind
	filter   filters.Pipeline

	mu      sync.Mutex
	offset  int64
	lineBuf []byte
}

// newStreamWriter constructs a writer that emits rawKind/lineKind
// runtime events for each Write. Pass [runtimeevents.KindStdoutRaw]
// + [runtimeevents.KindStdoutLine] for stdout, the stderr pair for
// stderr.
func newStreamWriter(
	ctx context.Context,
	bridge *activity.Bridge,
	source runtimeevents.Source,
	rawKind, lineKind runtimeevents.EventKind,
	filter filters.Pipeline,
) *streamWriter {
	return &streamWriter{
		bridge:   bridge,
		ctx:      ctx,
		source:   source,
		rawKind:  rawKind,
		lineKind: lineKind,
		filter:   filter,
	}
}

// Write implements io.Writer. Always reports len(p) bytes written
// (never partial) and a nil error — emitter errors are swallowed
// because returning a non-nil error from the agentkit-side Fanout
// writer can stall the reader goroutine.
func (s *streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	chunkOffset := s.offset
	s.offset += int64(len(p))

	// Emit the raw chunk first so byte-replay consumers see the
	// complete stream in order. Copy p — the agentkit reader is
	// allowed to reuse its buffer after Write returns.
	chunk := append([]byte(nil), p...)
	emitChunk := s.filterBytes("command_output", chunk)
	_ = s.bridge.Emit(s.ctx, s.rawKind, s.source, map[string]any{
		"bytes": string(emitChunk),
	}, runtimeevents.WithRawOffset(chunkOffset))

	// Append to the line buffer and emit one event per complete
	// line. Trailing partial line stays in lineBuf.
	s.lineBuf = append(s.lineBuf, chunk...)
	lineStart := chunkOffset // approximation for the first line's offset
	for {
		i := bytes.IndexByte(s.lineBuf, '\n')
		if i < 0 {
			break
		}
		line := s.lineBuf[:i]
		line = bytes.TrimSuffix(line, []byte{'\r'})
		emitLine := s.filterBytes("command_output_line", line)

		_ = s.bridge.Emit(s.ctx, s.lineKind, s.source, map[string]any{
			"line": string(emitLine),
		}, runtimeevents.WithRawOffset(lineStart))

		// Advance lineStart to just after the \n we consumed —
		// approximate offset for the next line.
		lineStart += int64(i) + 1
		s.lineBuf = s.lineBuf[i+1:]
	}

	return len(p), nil
}

func (s *streamWriter) filterBytes(kind string, b []byte) []byte {
	if s.filter == nil {
		return b
	}
	out, err := s.filter.Process(s.ctx, filters.Input{
		Kind:    kind,
		Content: append([]byte(nil), b...),
		Metadata: map[string]string{
			"event_kind": string(s.rawKind),
			"channel":    string(s.source.Channel),
		},
	})
	if err != nil || out.Repaired == nil {
		return b
	}
	return out.Repaired
}
