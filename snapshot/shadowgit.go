package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // matches git's own blob object hash algorithm, not used for security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxUntrackedFileSize is the default per-file ceiling (bytes) for a
// previously-untracked file to be included in a capture. Matches OpenCode's
// own field-tested default (2 MiB) — see the package doc and
// [SkipOversizeUntracked].
const DefaultMaxUntrackedFileSize = 2 * 1024 * 1024

// emptyTreeHash is git's well-known empty-tree object ID (SHA-1 object
// format — the default, and what [NewShadowGit] always initializes with).
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// snapshotRefPrefix namespaces every ref ShadowGit creates inside a
// Target's shadow git directory. Never under refs/heads/ — these are not
// branches meant to be checked out, just independently-collectable GC
// roots, one per capture.
const snapshotRefPrefix = "refs/snapshots/"

// ErrShadowStoreUnavailable wraps any error that means the shadow-store
// mechanism itself is broken for a Target: the base directory couldn't be
// created, `git init --bare` failed, the configured git binary isn't
// runnable, or an existing shadow store failed its identity check on open.
// Always a loud, non-best-effort failure — see the package doc, "Two
// failure modes, never conflated". Callers should prefer [errors.Is] over
// equality.
var ErrShadowStoreUnavailable = errors.New("snapshot: shadow store unavailable")

// ShadowGit is the reference [FilesystemSnapshotProvider] implementation.
// It keeps a separate internal git object database per [Target], under its
// own base directory, and never touches any real repository's .git. See
// the package doc for the isolation guarantees and failure-mode contract
// this type implements.
//
// A ShadowGit is safe for concurrent use by multiple goroutines within one
// process. It is not a cross-process lock — see the package doc, "Not a
// concurrency lock".
type ShadowGit struct {
	baseDir           string
	gitBin            string
	logger            *slog.Logger
	maxUntrackedBytes int64

	mu     sync.Mutex
	target map[string]*sync.Mutex // per-target-ID in-process serialization
}

// Option configures a [ShadowGit] at construction.
type Option func(*ShadowGit)

// WithLogger sets the logger ShadowGit uses for loud init/infrastructure
// failures and best-effort capture warnings (see the package doc). Nil is
// ignored. Defaults to [slog.Default].
func WithLogger(l *slog.Logger) Option {
	return func(s *ShadowGit) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithGitBinary overrides the "git" executable resolved from PATH. Mostly
// useful for tests.
func WithGitBinary(path string) Option {
	return func(s *ShadowGit) {
		if path != "" {
			s.gitBin = path
		}
	}
}

// WithMaxUntrackedFileSize overrides [DefaultMaxUntrackedFileSize].
func WithMaxUntrackedFileSize(n int64) Option {
	return func(s *ShadowGit) {
		if n > 0 {
			s.maxUntrackedBytes = n
		}
	}
}

// NewShadowGit constructs a ShadowGit rooted at baseDir. baseDir is created
// (mode 0700 — see the package doc's retention/security note) if it doesn't
// exist, and the configured git binary is verified runnable, both eagerly:
// NewShadowGit fails loudly here rather than deferring the failure to the
// first Capture, which is exactly the "shadow directory never initialized,
// capture silently produces nothing" field bug this package is designed
// against.
func NewShadowGit(baseDir string, opts ...Option) (*ShadowGit, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, fmt.Errorf("%w: base directory is required", ErrShadowStoreUnavailable)
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve base directory %q: %v", ErrShadowStoreUnavailable, baseDir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create base directory %q: %v", ErrShadowStoreUnavailable, abs, err)
	}
	sg := &ShadowGit{
		baseDir:           abs,
		gitBin:            "git",
		logger:            slog.Default(),
		maxUntrackedBytes: DefaultMaxUntrackedFileSize,
		target:            make(map[string]*sync.Mutex),
	}
	for _, opt := range opts {
		opt(sg)
	}
	if _, err := exec.LookPath(sg.gitBin); err != nil {
		return nil, fmt.Errorf("%w: git binary %q not runnable: %v", ErrShadowStoreUnavailable, sg.gitBin, err)
	}
	return sg, nil
}

// --- Capture -----------------------------------------------------------

// Capture implements [FilesystemSnapshotProvider].
func (sg *ShadowGit) Capture(ctx context.Context, targets []Target) (SnapshotSet, error) {
	set := SnapshotSet{CapturedAt: time.Now().UTC(), Roots: make(map[string]RootSnapshot, len(targets))}
	var infraErr error
	for _, t := range targets {
		if err := t.validate(); err != nil {
			set.Roots[t.ID] = RootSnapshot{TargetID: t.ID, Root: t.Root, Err: err}
			sg.logger.WarnContext(ctx, "snapshot: capture skipped invalid target", "target_id", t.ID, "error", err)
			continue
		}
		rs, err := sg.captureOne(ctx, t)
		if err != nil {
			if errors.Is(err, ErrShadowStoreUnavailable) {
				sg.logger.ErrorContext(ctx, "snapshot: shadow store infrastructure failure", "target_id", t.ID, "error", err)
				infraErr = errors.Join(infraErr, err)
				continue
			}
			// Best-effort: this Target's capture didn't land this time,
			// but the shadow store itself is fine. Never escalate — see
			// the package doc, "Two failure modes, never conflated".
			sg.logger.WarnContext(ctx, "snapshot: capture failed for target (best-effort, turn proceeds)", "target_id", t.ID, "error", err)
			set.Roots[t.ID] = RootSnapshot{TargetID: t.ID, Root: t.Root, Err: err}
			continue
		}
		set.Roots[t.ID] = rs
	}
	return set, infraErr
}

func (sg *ShadowGit) captureOne(ctx context.Context, t Target) (RootSnapshot, error) {
	lock := sg.lockForID(t.ID)
	lock.Lock()
	defer lock.Unlock()

	gitDir, err := sg.ensureShadowRepo(ctx, t.ID)
	if err != nil {
		return RootSnapshot{}, err // already wrapped in ErrShadowStoreUnavailable
	}

	if fi, statErr := os.Stat(t.Root); statErr != nil || !fi.IsDir() {
		if statErr == nil {
			statErr = fmt.Errorf("root %q is not a directory", t.Root)
		}
		return RootSnapshot{}, fmt.Errorf("target %q root unavailable: %w", t.ID, statErr)
	}

	includes := normalizeIncludes(t.IncludePaths)

	skipped, oversizeExcludes, err := sg.detectOversizeUntracked(ctx, gitDir, t.Root, includes)
	if err != nil {
		return RootSnapshot{}, fmt.Errorf("target %q: detect untracked files: %w", t.ID, err)
	}

	addArgs := append([]string{"add", "-A", "--"}, buildPathspecs(includes, oversizeExcludes)...)
	if _, err := sg.runGit(ctx, gitDir, t.Root, nil, addArgs...); err != nil {
		return RootSnapshot{}, fmt.Errorf("target %q: git add: %w", t.ID, err)
	}

	treeOut, err := sg.runGit(ctx, gitDir, t.Root, nil, "write-tree")
	if err != nil {
		return RootSnapshot{}, fmt.Errorf("target %q: git write-tree: %w", t.ID, err)
	}
	tree := strings.TrimSpace(string(treeOut))

	env := []string{
		"GIT_AUTHOR_NAME=nanite-snapshot", "GIT_AUTHOR_EMAIL=snapshot@localhost",
		"GIT_COMMITTER_NAME=nanite-snapshot", "GIT_COMMITTER_EMAIL=snapshot@localhost",
	}
	commitOut, err := sg.runGit(ctx, gitDir, "", env, "commit-tree", tree, "-m", "snapshot capture")
	if err != nil {
		return RootSnapshot{}, fmt.Errorf("target %q: git commit-tree: %w", t.ID, err)
	}
	commit := strings.TrimSpace(string(commitOut))

	refName, err := snapshotRefName(time.Now())
	if err != nil {
		return RootSnapshot{}, fmt.Errorf("target %q: mint snapshot ref: %w", t.ID, err)
	}
	if _, err := sg.runGit(ctx, gitDir, "", nil, "update-ref", refName, commit); err != nil {
		return RootSnapshot{}, fmt.Errorf("target %q: git update-ref: %w", t.ID, err)
	}

	return RootSnapshot{
		TargetID:   t.ID,
		Root:       t.Root,
		TreeHash:   tree,
		CommitHash: commit,
		Skipped:    skipped,
	}, nil
}

// --- Diff ----------------------------------------------------------------

// Diff implements [FilesystemSnapshotProvider].
func (sg *ShadowGit) Diff(ctx context.Context, from, to SnapshotSet) (Diff, error) {
	ids := unionKeys(from.Roots, to.Roots)
	result := Diff{Roots: make(map[string]RootDiff, len(ids))}
	for _, id := range ids {
		fr, fok := from.Roots[id]
		tr, tok := to.Roots[id]

		gitDir, err := sg.openShadowRepo(id)
		if err != nil {
			return Diff{}, fmt.Errorf("diff target %q: %w", id, err)
		}

		fromTree := emptyTreeHash
		if fok && fr.TreeHash != "" {
			fromTree = fr.TreeHash
		}
		toTree := emptyTreeHash
		if tok && tr.TreeHash != "" {
			toTree = tr.TreeHash
		}

		out, err := sg.runGit(ctx, gitDir, "", nil, "diff-tree", "-r", "--no-commit-id", "--name-status", fromTree, toTree)
		if err != nil {
			return Diff{}, fmt.Errorf("diff target %q: git diff-tree: %w", id, err)
		}
		files, err := parseNameStatus(out)
		if err != nil {
			return Diff{}, fmt.Errorf("diff target %q: %w", id, err)
		}

		root := tr.Root
		if root == "" {
			root = fr.Root
		}
		result.Roots[id] = RootDiff{TargetID: id, Root: root, Files: files}
	}
	return result, nil
}

// --- Preview / Restore ----------------------------------------------------

// Preview implements [FilesystemSnapshotProvider].
func (sg *ShadowGit) Preview(ctx context.Context, set SnapshotSet, paths []string) (Preview, error) {
	byTarget, order, err := groupPathsByTarget(paths)
	if err != nil {
		return Preview{}, err
	}
	result := Preview{Roots: make(map[string]RootPreview, len(order))}
	for _, targetID := range order {
		rs, ok := set.Roots[targetID]
		if !ok {
			return Preview{}, fmt.Errorf("snapshot: preview target %q not present in given SnapshotSet", targetID)
		}
		gitDir, err := sg.openShadowRepo(targetID)
		if err != nil {
			return Preview{}, fmt.Errorf("preview target %q: %w", targetID, err)
		}

		relPaths := byTarget[targetID]
		changes := make([]RestoreChange, 0, len(relPaths))
		for _, rel := range relPaths {
			if err := validateRestorePath(rel); err != nil {
				return Preview{}, fmt.Errorf("preview target %q: %w", targetID, err)
			}
			snapHash, _, snapExists, err := sg.blobAt(ctx, gitDir, rs.TreeHash, rel)
			if err != nil {
				return Preview{}, fmt.Errorf("preview target %q path %q: %w", targetID, rel, err)
			}
			curHash, curExists, err := sg.currentBlobHash(rs.Root, rel)
			if err != nil {
				return Preview{}, fmt.Errorf("preview target %q path %q: %w", targetID, rel, err)
			}
			changes = append(changes, RestoreChange{
				Path:         rel,
				Kind:         classifyRestore(curExists, snapExists, curHash, snapHash),
				CurrentHash:  curHash,
				SnapshotHash: snapHash,
			})
		}
		result.Roots[targetID] = RootPreview{TargetID: targetID, Root: rs.Root, Changes: changes}
	}
	return result, nil
}

// Restore implements [FilesystemSnapshotProvider]. See the package doc,
// "Restore is selective only" — there is no whole-tree code path; an empty
// paths restores nothing.
func (sg *ShadowGit) Restore(ctx context.Context, set SnapshotSet, paths []string) error {
	byTarget, order, err := groupPathsByTarget(paths)
	if err != nil {
		return err
	}
	var errs error
	for _, targetID := range order {
		rs, ok := set.Roots[targetID]
		if !ok {
			errs = errors.Join(errs, fmt.Errorf("snapshot: restore target %q not present in given SnapshotSet", targetID))
			continue
		}
		gitDir, err := sg.openShadowRepo(targetID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("restore target %q: %w", targetID, err))
			continue
		}

		lock := sg.lockForID(targetID)
		lock.Lock()
		for _, rel := range byTarget[targetID] {
			if err := sg.restoreOne(ctx, gitDir, rs, rel); err != nil {
				errs = errors.Join(errs, fmt.Errorf("restore target %q path %q: %w", targetID, rel, err))
			}
		}
		lock.Unlock()
	}
	return errs
}

func (sg *ShadowGit) restoreOne(ctx context.Context, gitDir string, rs RootSnapshot, rel string) error {
	if err := validateRestorePath(rel); err != nil {
		return err
	}
	hash, mode, exists, err := sg.blobAt(ctx, gitDir, rs.TreeHash, rel)
	if err != nil {
		return err
	}
	full := filepath.Join(rs.Root, filepath.FromSlash(rel))

	curHash, curExists, err := sg.currentBlobHash(rs.Root, rel)
	if err != nil {
		return err
	}

	if !exists {
		if !curExists {
			return nil // nothing on disk, nothing in the snapshot: no-op
		}
		sg.logger.WarnContext(ctx, "snapshot: restore removes path not present in snapshot", "path", rel, "root", rs.Root)
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", full, err)
		}
		return nil
	}

	if curExists && curHash == hash {
		return nil // already matches; skip the write so we never touch mtime for a no-op
	}
	if curExists {
		sg.logger.WarnContext(ctx, "snapshot: restore overwrites on-disk content that differs from the snapshot", "path", rel, "root", rs.Root, "current_hash", curHash, "snapshot_hash", hash)
	}

	content, err := sg.catBlob(ctx, gitDir, hash)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir for %q: %w", full, err)
	}
	if mode == "120000" {
		_ = os.Remove(full) // os.Symlink requires the target path not already exist
		if err := os.Symlink(string(content), full); err != nil {
			return fmt.Errorf("symlink %q: %w", full, err)
		}
		return nil
	}
	perm := os.FileMode(0o644)
	if mode == "100755" {
		perm = 0o755
	}
	if err := os.WriteFile(full, content, perm); err != nil {
		return fmt.Errorf("write %q: %w", full, err)
	}
	return nil
}

func classifyRestore(curExists, snapExists bool, curHash, snapHash string) ChangeKind {
	switch {
	case !curExists && !snapExists:
		return ChangeUnchanged
	case !curExists && snapExists:
		return ChangeAdded
	case curExists && !snapExists:
		return ChangeDeleted
	case curHash == snapHash:
		return ChangeUnchanged
	default:
		return ChangeModified
	}
}

func validateRestorePath(rel string) error {
	if rel == "" {
		return errors.New("snapshot: empty restore path")
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return fmt.Errorf("snapshot: unsafe restore path %q", rel)
	}
	return nil
}

// --- Cleanup / Purge -------------------------------------------------------

// Cleanup implements [Cleaner]. It removes captures for targetID that fall
// outside policy (age and/or count), then reclaims their objects via `git
// gc`. The policy values themselves are always caller-supplied — see
// [CleanupPolicy].
func (sg *ShadowGit) Cleanup(ctx context.Context, targetID string, policy CleanupPolicy) (CleanupResult, error) {
	gitDir, err := sg.openShadowRepo(targetID)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("cleanup target %q: %w", targetID, err)
	}

	lock := sg.lockForID(targetID)
	lock.Lock()
	defer lock.Unlock()

	out, err := sg.runGit(ctx, gitDir, "", nil, "for-each-ref", "--format=%(refname)", snapshotRefPrefix)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("cleanup target %q: list refs: %w", targetID, err)
	}

	type entry struct {
		ref string
		ts  time.Time
	}
	var entries []entry
	for _, ref := range splitLines(out) {
		ts, err := parseSnapshotRefTimestamp(ref)
		if err != nil {
			sg.logger.WarnContext(ctx, "snapshot: cleanup skipping unrecognized ref", "ref", ref, "error", err)
			continue
		}
		entries = append(entries, entry{ref: ref, ts: ts})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ts.After(entries[j].ts) }) // newest first

	now := time.Now()
	var toRemove []string
	for i, e := range entries {
		expired := policy.MaxAge > 0 && now.Sub(e.ts) > policy.MaxAge
		overCount := policy.MaxSnapshotSets > 0 && i >= policy.MaxSnapshotSets
		if expired || overCount {
			toRemove = append(toRemove, e.ref)
		}
	}

	for _, ref := range toRemove {
		if _, err := sg.runGit(ctx, gitDir, "", nil, "update-ref", "-d", ref); err != nil {
			return CleanupResult{}, fmt.Errorf("cleanup target %q: delete ref %q: %w", targetID, ref, err)
		}
	}
	if len(toRemove) > 0 {
		if _, err := sg.runGit(ctx, gitDir, "", nil,
			"-c", "gc.reflogExpire=now", "-c", "gc.reflogExpireUnreachable=now",
			"gc", "--prune=now", "--quiet"); err != nil {
			return CleanupResult{}, fmt.Errorf("cleanup target %q: gc: %w", targetID, err)
		}
	}

	return CleanupResult{
		TargetID:       targetID,
		Removed:        len(toRemove),
		RemainingCount: len(entries) - len(toRemove),
	}, nil
}

// Purge permanently deletes a Target's entire shadow store (all history,
// unconditionally) by removing its on-disk directory. Unlike [Cleanup],
// which is a policy-driven, ref-scoped trim, Purge is total — meant for
// "this project/agent grant is gone, forget it existed" rather than
// day-to-day retention. Safe to call even if nothing was ever captured for
// targetID.
func (sg *ShadowGit) Purge(_ context.Context, targetID string) error {
	lock := sg.lockForID(targetID)
	lock.Lock()
	defer lock.Unlock()

	dir := sg.shadowKeyDir(targetID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("%w: purge target %q: %v", ErrShadowStoreUnavailable, targetID, err)
	}
	return nil
}

// --- shadow store lifecycle -----------------------------------------------

type shadowMeta struct {
	TargetID  string    `json:"target_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (sg *ShadowGit) shadowKeyDir(targetID string) string {
	sum := sha256.Sum256([]byte(targetID))
	return filepath.Join(sg.baseDir, "targets", hex.EncodeToString(sum[:])[:32])
}

func (sg *ShadowGit) gitDirFor(targetID string) string {
	return filepath.Join(sg.shadowKeyDir(targetID), "shadow.git")
}

// ensureShadowRepo opens the shadow git directory for targetID, creating
// and initializing it on first use. Every error path here is wrapped in
// [ErrShadowStoreUnavailable] — see the package doc, "Two failure modes,
// never conflated".
func (sg *ShadowGit) ensureShadowRepo(ctx context.Context, targetID string) (string, error) {
	root := sg.shadowKeyDir(targetID)
	gitDir := sg.gitDirFor(targetID)
	metaPath := filepath.Join(root, "meta.json")

	if fi, err := os.Stat(gitDir); err == nil && fi.IsDir() {
		if err := sg.verifyMeta(metaPath, targetID); err != nil {
			return "", fmt.Errorf("%w: target %q: %v", ErrShadowStoreUnavailable, targetID, err)
		}
		return gitDir, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: stat shadow git dir for target %q: %v", ErrShadowStoreUnavailable, targetID, err)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("%w: create shadow store for target %q: %v", ErrShadowStoreUnavailable, targetID, err)
	}
	if out, err := sg.runGitRaw(ctx, root, nil, "-c", "init.defaultBranch=snapshot", "init", "--quiet", "--bare", gitDir); err != nil {
		return "", fmt.Errorf("%w: init shadow git dir for target %q: %v (%s)", ErrShadowStoreUnavailable, targetID, err, out)
	}
	meta := shadowMeta{TargetID: targetID, CreatedAt: time.Now().UTC()}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", fmt.Errorf("%w: encode shadow meta for target %q: %v", ErrShadowStoreUnavailable, targetID, err)
	}
	if err := os.WriteFile(metaPath, b, 0o600); err != nil {
		return "", fmt.Errorf("%w: write shadow meta for target %q: %v", ErrShadowStoreUnavailable, targetID, err)
	}
	return gitDir, nil
}

func (sg *ShadowGit) verifyMeta(metaPath, targetID string) error {
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("shadow store present but meta.json unreadable: %w", err)
	}
	var meta shadowMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return fmt.Errorf("shadow store meta.json unparsable: %w", err)
	}
	if meta.TargetID != targetID {
		return fmt.Errorf("shadow store key collision: on-disk meta target_id %q != requested %q", meta.TargetID, targetID)
	}
	return nil
}

// openShadowRepo looks up an already-initialized shadow git directory for
// targetID without creating one. Used by Diff/Preview/Restore/Cleanup,
// which operate against a previously captured SnapshotSet rather than
// initializing new state.
func (sg *ShadowGit) openShadowRepo(targetID string) (string, error) {
	gitDir := sg.gitDirFor(targetID)
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: no shadow store found for target %q under %q", ErrShadowStoreUnavailable, targetID, sg.baseDir)
	}
	if err := sg.verifyMeta(filepath.Join(sg.shadowKeyDir(targetID), "meta.json"), targetID); err != nil {
		return "", fmt.Errorf("%w: target %q: %v", ErrShadowStoreUnavailable, targetID, err)
	}
	return gitDir, nil
}

func (sg *ShadowGit) lockForID(id string) *sync.Mutex {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	l, ok := sg.target[id]
	if !ok {
		l = &sync.Mutex{}
		sg.target[id] = l
	}
	return l
}

// --- exclusion rules -------------------------------------------------------

// gitMetadataExcludes are always applied to every capture — see
// [SkipGitMetadata] and the package doc's working-directory/metadata
// isolation section.
var gitMetadataExcludes = []string{":(exclude).git", ":(exclude,glob)**/.git"}

func normalizeIncludes(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p == "" || p == "." {
			return nil // any entry meaning "everything" collapses IncludePaths to "everything"
		}
		out = append(out, p)
	}
	return out
}

func buildPathspecs(includes, oversizeExcludes []string) []string {
	specs := make([]string, 0, len(includes)+len(oversizeExcludes)+len(gitMetadataExcludes)+1)
	if len(includes) == 0 {
		specs = append(specs, ".")
	} else {
		specs = append(specs, includes...)
	}
	specs = append(specs, gitMetadataExcludes...)
	for _, p := range oversizeExcludes {
		specs = append(specs, ":(exclude)"+p)
	}
	return specs
}

// detectOversizeUntracked scopes [SkipOversizeUntracked] to files git
// doesn't already know about for this Target (see the field's own doc):
// git ls-files --others only ever reports untracked, non-ignored paths, so
// this is bounded by "how much new stuff showed up since the last
// capture", not by the whole tree's size.
func (sg *ShadowGit) detectOversizeUntracked(ctx context.Context, gitDir, root string, includes []string) ([]SkippedPath, []string, error) {
	args := []string{"ls-files", "--others", "--exclude-standard", "--"}
	if len(includes) == 0 {
		args = append(args, ".")
	} else {
		args = append(args, includes...)
	}
	out, err := sg.runGit(ctx, gitDir, root, nil, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("git ls-files --others: %w", err)
	}
	var skipped []SkippedPath
	var excludes []string
	for _, rel := range splitLines(out) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(full)
		if err != nil {
			continue // vanished between ls-files and stat; git add will see nothing there either
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue // the size ceiling only meaningfully applies to regular file content
		}
		if fi.Size() > sg.maxUntrackedBytes {
			skipped = append(skipped, SkippedPath{Path: rel, Reason: SkipOversizeUntracked, SizeBytes: fi.Size()})
			excludes = append(excludes, rel)
		}
	}
	return skipped, excludes, nil
}

// --- git object access -----------------------------------------------------

// blobAt looks up the blob for relPath inside tree. exists is false (with a
// nil error) if relPath isn't present in tree.
func (sg *ShadowGit) blobAt(ctx context.Context, gitDir, tree, relPath string) (hash, mode string, exists bool, err error) {
	if tree == "" {
		return "", "", false, nil
	}
	out, err := sg.runGit(ctx, gitDir, "", nil, "ls-tree", "-r", "--full-tree", tree, "--", relPath)
	if err != nil {
		return "", "", false, fmt.Errorf("git ls-tree: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", "", false, nil
	}
	tabIdx := strings.IndexByte(line, '\t')
	if tabIdx < 0 {
		return "", "", false, fmt.Errorf("unexpected ls-tree output %q", line)
	}
	fields := strings.Fields(line[:tabIdx])
	if len(fields) != 3 {
		return "", "", false, fmt.Errorf("unexpected ls-tree output %q", line)
	}
	return fields[2], fields[0], true, nil
}

func (sg *ShadowGit) catBlob(ctx context.Context, gitDir, hash string) ([]byte, error) {
	out, err := sg.runGit(ctx, gitDir, "", nil, "cat-file", "-p", hash)
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	return out, nil
}

// currentBlobHash reproduces git's own blob object hash (SHA-1, "blob
// <len>\x00<content>") for the file currently on disk at root/relPath,
// without shelling out to git — so it never depends on a given git
// version's own hash-object symlink-following default, and it's one fewer
// process spawn on what can be a hot path ([Preview]/[Restore] over many
// paths).
func (sg *ShadowGit) currentBlobHash(root, relPath string) (hash string, exists bool, err error) {
	full := filepath.Join(root, filepath.FromSlash(relPath))
	fi, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat %q: %w", full, err)
	}
	var content []byte
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return "", false, fmt.Errorf("readlink %q: %w", full, err)
		}
		content = []byte(target)
	case fi.IsDir():
		return "", false, fmt.Errorf("%q is a directory, not a file", full)
	default:
		content, err = os.ReadFile(full)
		if err != nil {
			return "", false, fmt.Errorf("read %q: %w", full, err)
		}
	}
	return gitBlobHash(content), true, nil
}

func gitBlobHash(content []byte) string {
	h := sha1.New() //nolint:gosec // git's object format, not a security boundary
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// --- git process isolation --------------------------------------------------

// runGit runs git against the given shadow git-dir (and, when non-empty,
// work-tree), fully isolated from ambient GIT_* environment and from the
// calling process's own working directory. See the package doc,
// "Working-directory / metadata isolation".
func (sg *ShadowGit) runGit(ctx context.Context, gitDir, workTree string, extraEnv []string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+2)
	full = append(full, "--git-dir="+gitDir)
	if workTree != "" {
		full = append(full, "--work-tree="+workTree)
	}
	full = append(full, args...)
	return sg.runGitRaw(ctx, sg.baseDir, extraEnv, full...)
}

// runGitRaw runs git with an explicit, GIT_*-stripped environment and an
// explicit cwd — never the zero value, which would silently inherit the
// calling process's own working directory (which could be inside a real
// repository, e.g. if this package is ever invoked from inside a git hook
// context).
func (sg *ShadowGit) runGitRaw(ctx context.Context, cwd string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, sg.gitBin, args...)
	cmd.Dir = cwd
	cmd.Env = isolatedEnv(extraEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// isolatedEnv builds a child-process environment from the current
// process's environment with every GIT_*-prefixed variable stripped, then
// layers extraEnv on top. This is the defense-in-depth half of
// working-directory isolation: every call site also passes
// --git-dir/--work-tree explicitly, so a stray inherited
// GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE (as a real git hook sets for its
// children) can never silently redirect a shadow operation at a real
// repository, even if a future call site forgets an explicit flag.
func isolatedEnv(extraEnv []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extraEnv))
	for _, kv := range base {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extraEnv...)
}

// --- small parsing/formatting helpers ---------------------------------------

func splitLines(b []byte) []string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func parseNameStatus(out []byte) ([]FileChange, error) {
	lines := splitLines(out)
	changes := make([]FileChange, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected diff-tree output line %q", line)
		}
		kind, err := changeKindFromStatus(parts[0])
		if err != nil {
			return nil, err
		}
		changes = append(changes, FileChange{Path: parts[1], Kind: kind})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

func changeKindFromStatus(code string) (ChangeKind, error) {
	if code == "" {
		return "", errors.New("empty diff-tree status code")
	}
	switch code[0] {
	case 'A':
		return ChangeAdded, nil
	case 'M':
		return ChangeModified, nil
	case 'D':
		return ChangeDeleted, nil
	default:
		return "", fmt.Errorf("unsupported diff-tree status %q", code)
	}
}

func unionKeys(a, b map[string]RootSnapshot) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// groupPathsByTarget splits [JoinPath]-encoded refs by target ID. order
// preserves each target's first-seen position for deterministic iteration
// (map iteration order isn't).
func groupPathsByTarget(paths []string) (byTarget map[string][]string, order []string, err error) {
	byTarget = make(map[string][]string)
	for _, p := range paths {
		targetID, rel, err := SplitPath(p)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := byTarget[targetID]; !ok {
			order = append(order, targetID)
		}
		byTarget[targetID] = append(byTarget[targetID], rel)
	}
	return byTarget, order, nil
}

func snapshotRefName(t time.Time) (string, error) {
	var nonce [2]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%020d-%s", snapshotRefPrefix, t.UnixNano(), hex.EncodeToString(nonce[:])), nil
}

func parseSnapshotRefTimestamp(ref string) (time.Time, error) {
	name := strings.TrimPrefix(ref, snapshotRefPrefix)
	dash := strings.IndexByte(name, '-')
	if dash < 0 {
		return time.Time{}, fmt.Errorf("malformed snapshot ref %q", ref)
	}
	nanos, err := strconv.ParseInt(name[:dash], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed snapshot ref %q: %w", ref, err)
	}
	return time.Unix(0, nanos), nil
}
