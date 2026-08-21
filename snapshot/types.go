package snapshot

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Target identifies one project root an agent is scoped to, and (optionally)
// the subset of that root worth capturing.
//
// One Target maps to one independent shadow store — see [ShadowGit]. A
// [SnapshotSet] spanning several Targets holds one tree hash per Target,
// never a single synthetic tree spanning unrelated roots. This mirrors
// Nanite's own `projects.repo_path` + `agent_projects` shape: one logical
// snapshot ("turn 42") is one capture per project root an agent is scoped
// to, not one flat path list across everything at once.
type Target struct {
	// ID stably identifies this root across captures (e.g. a project ID).
	// Required, non-empty. Keep it stable for the lifetime a caller wants
	// undo/diff history to stay connected — it is the only thing used to
	// key this Target's shadow store.
	ID string

	// Root is the absolute path to the real directory being snapshotted.
	// Required. This package only ever reads from and selectively writes
	// into Root; it never creates, moves, or removes Root itself.
	Root string

	// IncludePaths, if non-empty, restricts capture to these Root-relative
	// paths (and their descendants) instead of all of Root. Callers
	// typically derive this from an agent's sandbox write-allowlist —
	// this package does not derive scope itself (a product-layer concern),
	// it only honors whatever scope it's given. Empty/nil means "all of
	// Root", subject to the other exclusion rules (see [ShadowGit]).
	IncludePaths []string
}

func (t Target) validate() error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("snapshot: target ID is required")
	}
	if strings.TrimSpace(t.Root) == "" {
		return fmt.Errorf("snapshot: target %q: root is required", t.ID)
	}
	return nil
}

// SkipReason names why a capture candidate path was excluded.
type SkipReason string

const (
	// SkipGitMetadata excludes a target's own .git directory (and any
	// nested repository boundary within it) from capture. Always applied;
	// never configurable off.
	SkipGitMetadata SkipReason = "git_metadata"

	// SkipOversizeUntracked excludes an individual untracked file over the
	// configured size ceiling (2 MiB by default, see
	// [DefaultMaxUntrackedFileSize]) from capture. Only applies to files
	// not already present in a previous capture of this Target — a file
	// already being followed keeps being followed even if it later grows
	// past the ceiling, so capture never silently drops history for a
	// file it was already tracking.
	SkipOversizeUntracked SkipReason = "oversize_untracked"
)

// SkippedPath is one path a capture explicitly declined to include, with
// why.
//
// Only [SkipGitMetadata] and [SkipOversizeUntracked] are enumerated
// individually here — both are cheap, bounded sets. Gitignored paths and
// paths outside a Target's IncludePaths scope are excluded via git's own
// ignore engine and pathspec scoping respectively, and are deliberately
// *not* enumerated path by path: doing so for something like a large
// node_modules tree would turn a fast per-model-step capture into a slow
// one for no operational benefit. [ShadowGit.Diff] reports what actually
// changed between two captures, which is the operationally useful signal —
// not an exhaustive list of everything that was never a candidate.
type SkippedPath struct {
	Path      string
	Reason    SkipReason
	SizeBytes int64 // populated for SkipOversizeUntracked; zero otherwise
}

// RootSnapshot is one Target's result within a [SnapshotSet].
type RootSnapshot struct {
	TargetID string
	Root     string

	// TreeHash is the shadow tree object for this capture. Empty if Err is
	// non-nil.
	TreeHash string

	// CommitHash wraps TreeHash. Each capture is an independent, parentless
	// commit (not chained to this Target's previous capture) — see
	// [ShadowGit], "shadow history shape" — so it can be independently
	// garbage-collected by [Cleaner.Cleanup] without rewriting anything
	// else. Empty if Err is non-nil.
	CommitHash string

	Skipped []SkippedPath

	// Err records a capture failure scoped to this one Target within an
	// otherwise-successful multi-target Capture call. Per the "capture is
	// best-effort" contract (see the package doc), a non-nil Err here
	// never makes [ShadowGit.Capture]'s own returned error non-nil — the
	// shadow store itself is healthy, this one Target's capture just
	// didn't land this time (missing Root, a transient git failure, ...).
	Err error `json:"-"`
}

// SnapshotSet is one logical snapshot ("before step 3 of turn 42") across
// every [Target] a [FilesystemSnapshotProvider.Capture] call was given —
// one shadow tree hash per project root, never one synthetic tree spanning
// unrelated roots.
type SnapshotSet struct {
	// ID is caller-opaque; this package never mints or interprets one.
	// Nanite is expected to supply a stable identifier (e.g. a ULID) it
	// can correlate back to the turn/step that produced this SnapshotSet.
	// [ShadowGit.Capture] always returns ID == "" — set it afterward if
	// wanted.
	ID string

	CapturedAt time.Time

	// Roots is keyed by Target.ID.
	Roots map[string]RootSnapshot
}

// ChangeKind classifies one path's state in a [Diff] or [Preview].
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"

	// ChangeUnchanged only appears in a [Preview] (a [Diff] never lists a
	// path that didn't change) — it means restoring this path would be a
	// no-op.
	ChangeUnchanged ChangeKind = "unchanged"
)

// FileChange is one changed path between two captures of the same Target.
type FileChange struct {
	// Path is Root-relative, forward-slash separated (a git pathspec, not
	// an OS path).
	Path string
	Kind ChangeKind
}

// RootDiff is one Target's changed paths within a [Diff].
type RootDiff struct {
	TargetID string
	Root     string
	Files    []FileChange
}

// Diff is what changed between two [SnapshotSet]s, one [RootDiff] per
// Target present in either side.
type Diff struct {
	Roots map[string]RootDiff
}

// RestoreChange previews what restoring one path would do.
type RestoreChange struct {
	// Path is Root-relative, forward-slash separated.
	Path string
	Kind ChangeKind

	// CurrentHash / SnapshotHash are git blob object IDs of the on-disk
	// content and the snapshot's content respectively. Empty means "does
	// not exist" on that side.
	CurrentHash  string
	SnapshotHash string
}

// RootPreview is one Target's previewed restore within a [Preview].
type RootPreview struct {
	TargetID string
	Root     string
	Changes  []RestoreChange
}

// Preview reports what a [FilesystemSnapshotProvider.Restore] call with the
// same (set, paths) arguments would do, without doing it — one entry per
// requested path, including paths where restoring would be a no-op
// ([ChangeUnchanged]), so a caller gets a complete, predictable answer for
// exactly what it asked about.
type Preview struct {
	Roots map[string]RootPreview
}

// CleanupPolicy is the retention rule [Cleaner.Cleanup] applies to one
// Target's capture history. The host (this package) owns the mechanism;
// the policy values themselves are always caller-supplied — this package
// has no opinion on how long anything should be kept.
type CleanupPolicy struct {
	// MaxAge, if non-zero, removes captures older than this.
	MaxAge time.Duration
	// MaxSnapshotSets, if non-zero, keeps only the N most recently
	// captured entries for the Target, removing the rest regardless of
	// age.
	MaxSnapshotSets int
}

// CleanupResult reports what [Cleaner.Cleanup] did.
type CleanupResult struct {
	TargetID string
	// Removed is the number of captures deleted.
	Removed int
	// RemainingCount is the number of captures still retained for the
	// Target after cleanup.
	RemainingCount int
}

// pathRefSep separates the TargetID and Root-relative-path halves of a
// [JoinPath]-produced string. A NUL byte can't appear in a real filesystem
// path and is exceedingly unlikely to appear in a caller-chosen TargetID,
// so this is unambiguous in practice; callers are expected to always go
// through [JoinPath] rather than hand-build the string, which is what
// actually guarantees correctness here, not the specific separator choice.
const pathRefSep = "\x00"

// JoinPath encodes a (targetID, relPath) pair into the single opaque string
// [FilesystemSnapshotProvider.Preview] and [FilesystemSnapshotProvider.Restore]
// expect in their paths argument. Both methods operate against a
// [SnapshotSet] that can span multiple Targets, but their fixed signature
// only takes a flat []string — JoinPath/[SplitPath] is this package's
// answer to carrying the per-root shape through that flat parameter.
// relPath must be Root-relative and forward-slash separated (a git
// pathspec).
func JoinPath(targetID, relPath string) string {
	return targetID + pathRefSep + relPath
}

// SplitPath decodes a string produced by [JoinPath]. Returns an error if s
// wasn't produced by JoinPath.
func SplitPath(s string) (targetID, relPath string, err error) {
	i := strings.Index(s, pathRefSep)
	if i < 0 {
		return "", "", fmt.Errorf("snapshot: %q is not a valid path ref (build it with snapshot.JoinPath)", s)
	}
	return s[:i], s[i+1:], nil
}
