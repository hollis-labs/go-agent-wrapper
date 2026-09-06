package wrapper

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/agentkit/materialize"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
	sandboxprofile "github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	pevents "github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

var (
	ErrPreparedExecutionConflict    = errors.New("wrapper: prepared execution inputs conflict")
	ErrPreparedPlantConflict        = errors.New("wrapper: prepared materialization conflicts with wrapper planter")
	ErrACPSandboxProfileUnsupported = errors.New("wrapper: ACP adapters require resolved SandboxPolicy; legacy SandboxProfile is unsupported")
)

func (w *Wrapper) defaultBootDir() string {
	if w.cfg.BootDir != "" {
		return w.cfg.BootDir
	}
	return filepath.Join(w.cfg.Workdir, ".wrapper-boot", w.sessionID)
}

func (w *Wrapper) resolvePreparedExecution(ctx context.Context, bootDir string) (*agentlaunch.PreparedExecution, error) {
	if w.cfg.PreparedExecution != nil && w.cfg.PrepareRequest != nil {
		return nil, ErrPreparedExecutionConflict
	}
	if w.cfg.PreparedExecution != nil {
		if err := w.cfg.PreparedExecution.Validate(); err != nil {
			return nil, fmt.Errorf("wrapper: prepared execution: %w", err)
		}
		return w.cfg.PreparedExecution, nil
	}
	if w.cfg.PrepareRequest == nil {
		return nil, nil
	}
	req := *w.cfg.PrepareRequest
	if req.Roots.ProjectRoot == "" {
		req.Roots.ProjectRoot = w.cfg.Workdir
	}
	if req.Roots.CWD == "" {
		req.Roots.CWD = w.cfg.Workdir
	}
	if req.Roots.BootRoot == "" {
		req.Roots.BootRoot = bootDir
	}
	if req.Projection.Bindings.CWD == "" {
		req.Projection.Bindings.CWD = req.Roots.CWD
	}
	prepared, err := agentlaunch.ResolvePreparation(ctx, req, agentlaunch.WithMaterializationEngine(w.cfg.MaterializationEngine))
	if err != nil {
		return nil, fmt.Errorf("wrapper: resolve preparation: %w", err)
	}
	if err := prepared.Validate(); err != nil {
		return nil, fmt.Errorf("wrapper: prepared execution: %w", err)
	}
	return prepared, nil
}

func preparedEnvSlice(prepared *agentlaunch.PreparedExecution) []string {
	if prepared == nil {
		return nil
	}
	keys := make([]string, 0, len(prepared.Bindings.Env))
	for key := range prepared.Bindings.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+prepared.Bindings.Env[key].Value)
	}
	return out
}

func preparedLaunchCommand(prepared *agentlaunch.PreparedExecution) *acp.LaunchCommand {
	if prepared == nil {
		return nil
	}
	args := append([]string(nil), prepared.Bindings.Argv[1:]...)
	return &acp.LaunchCommand{Binary: prepared.Bindings.Argv[0], Args: args}
}

func acpSandboxPolicyFromPrepared(prepared *agentlaunch.PreparedExecution, fallbackCWD string) (*sandboxprofile.ResolvedAccessPolicy, error) {
	if prepared == nil {
		return nil, nil
	}
	return agentsessions.PreparedSandboxPolicy(prepared, fallbackCWD)
}

func preparedCLIAdapter(inner provider.CLIAdapter, prepared *agentlaunch.PreparedExecution) (provider.CLIAdapter, error) {
	if prepared == nil {
		return inner, nil
	}
	if err := prepared.Validate(); err != nil {
		return nil, err
	}
	return &preparedAdapter{inner: inner, binary: prepared.Bindings.Argv[0]}, nil
}

type preparedAdapter struct {
	inner  provider.CLIAdapter
	binary string
}

func (a *preparedAdapter) Name() string { return a.inner.Name() }
func (a *preparedAdapter) Detect() (string, bool) {
	return a.binary, a.binary != ""
}
func (a *preparedAdapter) BuildArgs(_, _, _ string) []string { return nil }
func (a *preparedAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	return a.inner.ParseLine(line)
}
func (a *preparedAdapter) ParseLineEvents(line []byte) ([]pevents.Event, error) {
	parser, ok := a.inner.(provider.EventParser)
	if !ok {
		return nil, nil
	}
	return parser.ParseLineEvents(line)
}

func emitPreparedMaterialization(ctx context.Context, bridgeActivity *activity.Bridge, source runtimeevents.Source, handle *materialize.Handle) {
	if handle == nil || bridgeActivity == nil {
		return
	}
	payload := materializationPayload(handle)
	_ = bridgeActivity.Emit(ctx, runtimeevents.KindPlantCompleted, source, payload)
}

func materializationPayload(handle *materialize.Handle) map[string]any {
	payload := map[string]any{
		"boot_dir":  handle.TargetRoot,
		"operation": string(handle.Report.Operation),
		"complete":  handle.Report.Complete,
	}
	planned := make([]string, 0, len(handle.Manifest.Entries))
	written := []string{}
	unchanged := []string{}
	conflicts := []string{}
	for _, entry := range handle.Manifest.Entries {
		planned = append(planned, filepath.Join(handle.TargetRoot, filepath.FromSlash(entry.Path)))
	}
	for _, change := range handle.Report.Changes {
		abs := filepath.Join(handle.TargetRoot, filepath.FromSlash(change.Path))
		switch change.Kind {
		case materialize.ChangeUnchanged:
			unchanged = append(unchanged, abs)
		case materialize.ChangeConflict:
			conflicts = append(conflicts, abs)
		default:
			written = append(written, abs)
		}
	}
	payload["planned_files"] = planned
	payload["written_files"] = written
	payload["unchanged_files"] = unchanged
	payload["conflict_files"] = conflicts
	payload["planted_files"] = written
	return payload
}

func emitSandboxOutcome(ctx context.Context, bridgeActivity *activity.Bridge, source runtimeevents.Source, out agentsessions.SandboxOutcome) {
	if bridgeActivity == nil {
		return
	}
	_ = bridgeActivity.Emit(ctx, runtimeevents.KindSandboxApplied, source, sandboxOutcomePayload(out, ""))
}

func sandboxOutcomePayload(out agentsessions.SandboxOutcome, errText string) map[string]any {
	payload := map[string]any{
		"policy_id":     out.PolicyID,
		"mode":          string(out.Mode),
		"backend":       string(out.Backend),
		"state":         string(out.State),
		"enforced":      out.Enforced,
		"disabled":      out.Disabled,
		"unsupported":   append([]string(nil), out.Unsupported...),
		"diagnostics":   append([]string(nil), out.Diagnostics...),
		"backend_goos":  out.BackendGOOS,
		"backend_ready": out.BackendReady,
		"legacy":        out.Legacy,
		"applied":       out.Enforced,
	}
	if errText != "" {
		payload["error"] = errText
	}
	return payload
}

func emitACPSandboxOutcome(ctx context.Context, bridgeActivity *activity.Bridge, source runtimeevents.Source, out sandboxprofile.EnforcementOutcome) {
	if bridgeActivity == nil {
		return
	}
	unsupported := make([]string, 0, len(out.Unsupported))
	for _, cap := range out.Unsupported {
		unsupported = append(unsupported, string(cap))
	}
	payload := map[string]any{
		"policy_id":     out.PolicyID,
		"mode":          string(out.Mode),
		"backend":       string(out.Backend),
		"state":         string(out.State),
		"enforced":      out.Enforced,
		"disabled":      out.Disabled,
		"unsupported":   unsupported,
		"diagnostics":   append([]string(nil), out.Diagnostics...),
		"backend_goos":  out.BackendGOOS,
		"backend_ready": out.BackendReady,
		"legacy":        false,
		"applied":       out.Enforced,
	}
	_ = bridgeActivity.Emit(ctx, runtimeevents.KindSandboxApplied, source, payload)
}

func emitRuntimeStartSandboxError(ctx context.Context, bridgeActivity *activity.Bridge, source runtimeevents.Source, err error) {
	if bridgeActivity == nil {
		return
	}
	var sandboxErr *agentsessions.SandboxError
	if !errors.As(err, &sandboxErr) {
		return
	}
	_ = bridgeActivity.Emit(ctx, runtimeevents.KindSandboxApplied, source, sandboxOutcomePayload(sandboxErr.Outcome, sandboxErr.Error()))
}
