package snapshot

import (
	"context"
	"time"
)

// FilesystemSnapshotProvider is the host's undo/audit primitive for the
// filesystem state a [Target]'s granted paths hold. See the package doc for
// what this is and — just as importantly — what it deliberately is not.
//
// [ShadowGit] is the first (only) implementation. The interface is named
// around intent (capture/diff/preview/restore), not mechanism, so a future
// non-git implementation (APFS snapshots, overlayfs/btrfs/ZFS,
// copy-on-write workspaces) can slot in later without this interface
// changing.
type FilesystemSnapshotProvider interface {
	// Capture takes one snapshot per Target. Best-effort per-Target: see
	// the package doc's "Two failure modes, never conflated" — a single
	// Target's capture failing is reported via that Target's
	// [RootSnapshot.Err], not via Capture's own returned error, unless the
	// shadow-store mechanism itself is broken.
	Capture(ctx context.Context, targets []Target) (SnapshotSet, error)

	// Diff reports what changed between two SnapshotSets, per Target
	// present in either one. Unlike Capture, Diff is not best-effort: a
	// missing or unreadable shadow store for a referenced Target is a real
	// error.
	Diff(ctx context.Context, from, to SnapshotSet) (Diff, error)

	// Preview reports what Restore(ctx, set, paths) would do without doing
	// it. paths are [JoinPath]-encoded (targetID, relPath) pairs.
	Preview(ctx context.Context, set SnapshotSet, paths []string) (Preview, error)

	// Restore selectively writes the given paths back to their content in
	// set. paths are [JoinPath]-encoded (targetID, relPath) pairs. There is
	// no whole-tree restore — an empty paths restores nothing, never
	// "everything". See the package doc, "Restore is selective only".
	Restore(ctx context.Context, set SnapshotSet, paths []string) error
}

// Cleaner is the mechanism side of shadow-store retention: a caller decides
// the policy (how long to keep a Target's capture history, or how many
// captures to keep); the host applies it. Implemented by [ShadowGit].
// Separate from [FilesystemSnapshotProvider] because retention is a
// lifecycle concern, not a per-turn one.
type Cleaner interface {
	Cleanup(ctx context.Context, targetID string, policy CleanupPolicy) (CleanupResult, error)
}

// NoOpProvider captures nothing and reports empty results everywhere. Use as
// a placeholder when a caller is wired for [FilesystemSnapshotProvider] but
// has no snapshot provider configured — the same role
// [sandbox.NoOpApplier]/[plant.NoOpPlanter] play for their packages.
type NoOpProvider struct{}

// Capture implements [FilesystemSnapshotProvider].
func (NoOpProvider) Capture(_ context.Context, _ []Target) (SnapshotSet, error) {
	return SnapshotSet{CapturedAt: time.Now(), Roots: map[string]RootSnapshot{}}, nil
}

// Diff implements [FilesystemSnapshotProvider].
func (NoOpProvider) Diff(_ context.Context, _, _ SnapshotSet) (Diff, error) {
	return Diff{Roots: map[string]RootDiff{}}, nil
}

// Preview implements [FilesystemSnapshotProvider].
func (NoOpProvider) Preview(_ context.Context, _ SnapshotSet, _ []string) (Preview, error) {
	return Preview{Roots: map[string]RootPreview{}}, nil
}

// Restore implements [FilesystemSnapshotProvider].
func (NoOpProvider) Restore(_ context.Context, _ SnapshotSet, _ []string) error {
	return nil
}
