// Package snapshot defines the wrapper's filesystem undo/audit primitive:
// capture a project tree before/after a model step, diff two captures,
// preview a selective restore, and apply it. See
// docs/engineering/architecture/18-filesystem-snapshots.md in the Nanite
// repo for the full design writeup this package implements; this comment
// only covers what a reader of this code specifically needs.
//
// # Not process rollback, not a backup system, not session recovery
//
// This is scoped narrowly to the filesystem content of an agent's granted
// paths. It is not "undo everything this process did anywhere" (that would
// need filesystem virtualization or copy-on-write sandboxing, a different
// and much harder system). It is not a backup system — there is no
// off-host copy, no encryption-at-rest guarantee beyond the host
// filesystem's own, and no promise of surviving beyond whatever retention
// the caller configures via [Cleaner.Cleanup] or [ShadowGit.Purge]. And it
// has zero coupling to conversation/session-state recovery (compaction,
// Glass-4 handoffs, session handoffs) — those stay a completely separate
// timeline with a completely separate recovery mechanism elsewhere in the
// host. A caller is free to *correlate* the two timelines in its own UI
// ("restore files touched after this point in the conversation"), but this
// package has no idea what a conversation, turn, or session even is.
//
// # Naming: "snapshot," never "checkpoint"
//
// This package is deliberately never called "checkpoint" anywhere in its
// API. `checkpoint` already names a different, unrelated concept elsewhere
// in this portfolio (a provider-session resume hint: native-resume vs.
// fresh-boot vs. unsupported). See the Nanite repo's
// docs/engineering/GLOSSARY.md, "Snapshot (filesystem)" entry, for the full
// three-way disambiguation against `checkpoint` and against Glass-4's own
// unrelated use of the plain word "snapshot" for a conversation-continuity
// artifact.
//
// # Host boundary: mechanism vs. policy
//
// Continuing this wrapper's existing policy/mechanism split (see the
// [policy] package's own doc comment, or the "sandbox"/"plant" packages
// alongside this one): this package owns capture, diff, preview, restore,
// target resolution within a given [Target], and shadow-store
// lifecycle/cleanup mechanism. It does not decide *when* to capture (a
// caller drives that — typically once immediately before a model call and
// once after each cleanly completed step, per OpenCode's own field-tested
// cadence) or *how long* to retain a capture (a caller supplies a
// [CleanupPolicy]; this package only applies it).
//
// # Two failure modes, never conflated
//
// [ShadowGit] distinguishes two genuinely different kinds of failure:
//
//  1. Shadow-store infrastructure failure: the shadow directory can't be
//     created or opened, the configured git binary isn't runnable, or an
//     existing shadow store fails an identity check on open. This is
//     always loud — wrapped in [ErrShadowStoreUnavailable] and returned as
//     a real, non-nil error from [NewShadowGit] or [ShadowGit.Capture].
//     Never a silent no-op: a shadow store that was never actually
//     initialized must not quietly produce zero snapshot data forever,
//     which is a real field bug this design is built against.
//  2. A single capture attempt failing for one [Target] within an
//     otherwise-healthy shadow store (its Root vanished, a transient git
//     error, a permission hiccup reading one file). This is best-effort by
//     design — matching this wrapper's existing "hints, not control"
//     principle, a failed capture must never block the model step it was
//     trying to snapshot. [ShadowGit.Capture] reports this per-[Target] via
//     [RootSnapshot.Err] and keeps [ShadowGit.Capture]'s own returned error
//     nil (or, for a multi-target batch, leaves the still-succeeding
//     targets fully usable) rather than escalating it.
//
// # Working-directory / metadata isolation
//
// ShadowGit keeps a completely separate internal git object database per
// Target, under its own base directory, and never touches any real
// repository's .git. Every git invocation this package makes explicitly
// passes --git-dir/--work-tree as absolute-path flags (never relying on an
// inherited GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE), explicitly strips any
// GIT_*-prefixed variable from the child process's environment before
// adding back only the ones this package sets itself, and always sets an
// explicit process working directory rooted inside the shadow store — never
// the zero value (which would silently inherit the calling process's own
// cwd) and never a path inside any real repository's working tree. This is
// deliberately paranoid: a real, previously reported field bug had a
// shadow-git-style tool corrupt a real repository's index when invoked from
// inside that repository's own pre-commit hook, almost certainly because a
// child git invocation inherited the hook's own GIT_DIR/GIT_WORK_TREE
// environment instead of using its own. This package's tests exercise both
// the plain case (a real .git present alongside the shadow store's target)
// and the env-leakage case (GIT_DIR/GIT_WORK_TREE poisoned in the calling
// process's own environment, as a real git hook would set for its
// children) directly against a real on-disk repository.
//
// # Restore is selective only — no whole-tree code path exists
//
// [FilesystemSnapshotProvider.Restore] always takes an explicit,
// caller-supplied list of paths. There is no "restore everything in this
// SnapshotSet" call, method, or internal helper anywhere in this package —
// not as a configurable default, a capability that deliberately does not
// exist. A whole-tree restore touches every restored file's mtime
// unconditionally, which is a real, previously reported field bug
// (breaking editor reload state and build-tool caches); selective restore
// only touches a path's mtime when that path's content actually changes
// (see [ShadowGit.Restore]'s no-op-on-match behavior).
//
// # Not a concurrency lock, and snapshots are not secret-free
//
// Capture takes no lock against concurrent external edits to the paths it
// captures — an edit landing between a capture and a later restore of the
// same path isn't prevented, only detected after the fact via the
// lightweight current-vs-snapshot hash comparison [ShadowGit.Preview] and
// [ShadowGit.Restore] both perform (see [RestoreChange]). Real
// conflict/concurrency handling beyond that comparison is out of scope for
// this package.
//
// Snapshots are not "secret-free": a captured shadow tree holds complete
// file content, so anything sensitive that ever touched a captured file
// (an API key pasted into a config file mid-turn, say) persists in the
// shadow store until [Cleaner.Cleanup] or [ShadowGit.Purge] actually
// removes it. [NewShadowGit] creates its base directory (and every
// per-target subdirectory) with mode 0700 as a baseline, but that is host
// filesystem permissions only — it is not encryption, and it is not a
// substitute for a caller applying the same access-control discipline to
// the shadow store that it already applies to other host-owned artifacts
// holding sensitive content (the boot dir, the sandbox profile). A caller
// with sensitive-data handling requirements should configure retention
// accordingly rather than assume this store is somehow exempt.
package snapshot
