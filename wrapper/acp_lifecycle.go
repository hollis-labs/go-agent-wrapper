package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func (w *Wrapper) runACP(ctx context.Context, desc adapters.Descriptor) error {
	if w.cfg.Workdir == "" {
		return errors.New("wrapper: Config.Workdir is required")
	}
	if desc.Transport != adapters.TransportStdio && desc.Transport != adapters.TransportTCP {
		return fmt.Errorf("%w: protocol=%q transport=%q", ErrUnknownRuntime, desc.Protocol, desc.Transport)
	}
	adapter, ok := w.cfg.Adapter.(acp.ClientAdapter)
	if !ok {
		return fmt.Errorf("%w: adapter %q", ErrAdapterNotACPClient, w.cfg.Adapter.Name())
	}

	w.cfg.Activity.Bind(w.cfg.App, w.sessionID, runtimeevents.Process{
		Provider: desc.Provider,
		Runtime:  acpRuntimeToken(desc.Transport),
	})
	source := runtimeevents.Source{Channel: runtimeevents.ChannelJSONRPC, Confidence: runtimeevents.ConfidenceExact}
	rawSource := source
	manager := w.cfg.ACPManager
	if manager == nil {
		manager = acp.NewManager()
	}
	w.sessMu.Lock()
	w.typedSource = source
	w.rawSource = rawSource
	w.acpManager = manager
	w.sessMu.Unlock()

	if err := w.runPlanter(ctx, source); err != nil {
		return err
	}

	launch := acp.LaunchParams{
		Cwd:             w.cfg.Workdir,
		SystemPrompt:    w.cfg.SystemPrompt,
		SessionIDPreset: w.cfg.SessionIDPreset,
		AuthMethodID:    w.cfg.ACPAuthMethodID,
		SessionModeID:   w.cfg.ACPSessionModeID,
		SessionConfig:   cloneSessionConfig(w.cfg.ACPSessionConfig),
		OnDiagnostic:    w.cfg.OnACPDiagnostic,
	}
	session, err := manager.Launch(ctx, acp.SessionConfig{
		ID: w.sessionID, Client: adapter.ACPClient(), Launch: launch,
	})
	if err != nil {
		return fmt.Errorf("wrapper: ACP launch: %w", err)
	}
	w.sessMu.Lock()
	w.acpSession = session
	w.sessMu.Unlock()
	if providerID := session.ProviderSessionID(); providerID != "" {
		w.cfg.Activity.Emitter().SetProviderSessionID(providerID)
		if w.cfg.OnSessionID != nil {
			w.cfg.OnSessionID(providerID)
		}
	}

	if w.cfg.AutoFireFirstTurn {
		if err := session.Prompt(ctx, w.cfg.FirstTurnPayload); err != nil {
			_ = session.Close(context.Background())
			return fmt.Errorf("wrapper: ACP first prompt: %w", err)
		}
	}

	heartbeatStop := make(chan struct{})
	if w.cfg.HeartbeatInterval > 0 {
		go func() {
			ticker := time.NewTicker(w.cfg.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionHeartbeat, source,
						map[string]any{"last_activity_at": time.Now().UTC()})
				case <-heartbeatStop:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	defer close(heartbeatStop)

	eventsDone := make(chan struct{})
	var sawProcessExit atomic.Bool
	go func() {
		defer close(eventsDone)
		for ev := range session.Events() {
			if providerID := session.ProviderSessionID(); providerID != "" {
				w.cfg.Activity.Emitter().SetProviderSessionID(providerID)
			}
			eventID := ev.ID
			if eventID == "" {
				eventID = runtimeevents.NewEventID()
			}
			opts := []runtimeevents.EmitOption{runtimeevents.WithID(eventID)}
			if ev.ParentID != "" {
				opts = append(opts, runtimeevents.WithParentID(ev.ParentID))
			}
			if ev.TurnID != "" {
				opts = append(opts, runtimeevents.WithTurnID(ev.TurnID))
			}
			var payload any
			if len(ev.Payload) > 0 {
				payload = json.RawMessage(ev.Payload)
			}
			_ = w.cfg.Activity.Emit(ctx, ev.Kind, source, payload, opts...)
			switch ev.Kind {
			case runtimeevents.KindProcessExited:
				sawProcessExit.Store(true)
			case runtimeevents.KindTurnStarted:
				_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionProcessing, source,
					map[string]any{"turn_id": ev.TurnID}, runtimeevents.WithTurnID(ev.TurnID))
			case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
				_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionIdle, source,
					map[string]any{"turn_id": ev.TurnID}, runtimeevents.WithTurnID(ev.TurnID))
			}
			if ev.Kind == runtimeevents.KindAgentToolUse {
				w.observeACPToolUsePolicy(ctx, source, ev.Payload, eventID, ev.TurnID)
			}
		}
	}()

	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = w.requestACPInterrupt(context.Background(), source, session, "ctx_cancel", true)
		case <-stopWatcher:
		}
	}()

	waitErr := session.Wait(context.Background())
	close(stopWatcher)
	<-eventsDone
	if !sawProcessExit.Load() {
		snapshot := session.Snapshot()
		payload := map[string]any{"outcome": snapshot.TerminalOutcome}
		if snapshot.Err != nil {
			payload["error"] = snapshot.Err.Error()
		}
		_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindProcessExited, source, payload)
	}
	if waitErr != nil {
		return fmt.Errorf("wrapper: ACP session ended: %w", waitErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return nil
}

func cloneSessionConfig(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func acpRuntimeToken(transport adapters.Transport) string {
	if transport == adapters.TransportTCP {
		return RuntimeACPTCP
	}
	return RuntimeACPStdio
}

func (w *Wrapper) requestACPInterrupt(
	ctx context.Context,
	source runtimeevents.Source,
	session *acp.Session,
	reason string,
	closeSession bool,
) error {
	requestID := runtimeevents.NewEventID()
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindInterruptRequested, source,
		map[string]any{"reason": reason}, runtimeevents.WithID(requestID))
	var err error
	if closeSession {
		err = session.Close(ctx)
	} else {
		err = session.Cancel(ctx)
	}
	ack := map[string]any{"reason": reason}
	if err != nil {
		ack["error"] = err.Error()
	}
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindInterruptAcknowledged, source,
		ack, runtimeevents.WithParentID(requestID))
	return err
}
