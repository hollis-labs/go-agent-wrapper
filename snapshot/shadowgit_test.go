package snapshot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// --- test helpers -----------------------------------------------------------

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@localhost",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// newRealRepo creates a real git repository at dir with an initial commit,
// simulating "a directory that also has a real .git" for the
// working-directory-isolation tests.
func newRealRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("real repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", "README.md")
	runGitT(t, dir, "commit", "--quiet", "-m", "init")
}

func newLoggerT(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newShadowGitT(t *testing.T) *ShadowGit {
	t.Helper()
	sg, err := NewShadowGit(t.TempDir(), WithLogger(newLoggerT(t)))
	if err != nil {
		t.Fatalf("NewShadowGit: %v", err)
	}
	return sg
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileExists(dir, rel string) bool {
	_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

func sortedPaths(changes []FileChange) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = string(c.Kind) + ":" + c.Path
	}
	sort.Strings(out)
	return out
}

// --- Capture: exclusion rules ------------------------------------------------

func TestCapture_ExcludesGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "*.log\n")
	writeFile(t, root, "keep.txt", "kept")
	writeFile(t, root, "ignored.log", "should not be captured")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-a", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	rs := set.Roots["proj-a"]
	if rs.Err != nil {
		t.Fatalf("capture error: %v", rs.Err)
	}

	gitDir := sg.gitDirFor("proj-a")
	out := runGitT(t, root, "--git-dir="+gitDir, "ls-tree", "-r", "--name-only", rs.TreeHash)
	if strings.Contains(out, "ignored.log") {
		t.Errorf("captured tree includes gitignored file:\n%s", out)
	}
	if !strings.Contains(out, "keep.txt") {
		t.Errorf("captured tree missing keep.txt:\n%s", out)
	}
}

func TestCapture_ExcludesGitMetadata(t *testing.T) {
	root := t.TempDir()
	newRealRepo(t, root) // gives root a real .git directory
	writeFile(t, root, "app.go", "package main")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-b", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	rs := set.Roots["proj-b"]
	if rs.Err != nil {
		t.Fatalf("capture error: %v", rs.Err)
	}

	gitDir := sg.gitDirFor("proj-b")
	out := runGitT(t, root, "--git-dir="+gitDir, "ls-tree", "-r", "--name-only", rs.TreeHash)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == ".git" || strings.HasPrefix(line, ".git/") {
			t.Errorf("captured tree includes real repo's git metadata: %q\nfull tree:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "app.go") {
		t.Errorf("captured tree missing app.go:\n%s", out)
	}
}

func TestCapture_ExcludesOversizeUntracked(t *testing.T) {
	root := t.TempDir()
	small := strings.Repeat("a", 100)
	big := strings.Repeat("b", 2*1024*1024+1) // just over the 2 MiB ceiling
	writeFile(t, root, "small.txt", small)
	writeFile(t, root, "big.bin", big)

	sg := newShadowGitT(t)
	target := Target{ID: "proj-c", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	rs := set.Roots["proj-c"]
	if rs.Err != nil {
		t.Fatalf("capture error: %v", rs.Err)
	}

	gitDir := sg.gitDirFor("proj-c")
	out := runGitT(t, root, "--git-dir="+gitDir, "ls-tree", "-r", "--name-only", rs.TreeHash)
	if strings.Contains(out, "big.bin") {
		t.Errorf("captured tree includes oversize untracked file:\n%s", out)
	}
	if !strings.Contains(out, "small.txt") {
		t.Errorf("captured tree missing small.txt:\n%s", out)
	}

	found := false
	for _, sp := range rs.Skipped {
		if sp.Path == "big.bin" && sp.Reason == SkipOversizeUntracked {
			found = true
			if sp.SizeBytes != int64(len(big)) {
				t.Errorf("SizeBytes = %d, want %d", sp.SizeBytes, len(big))
			}
		}
	}
	if !found {
		t.Errorf("Skipped does not record big.bin as SkipOversizeUntracked: %+v", rs.Skipped)
	}

	// Second capture: a file that's already tracked keeps being tracked
	// even after growing past the ceiling.
	writeFile(t, root, "small.txt", small) // re-touch, unrelated
	writeFile(t, root, "grows.txt", "short")
	set2, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture (2): %v", err)
	}
	rs2 := set2.Roots["proj-c"]
	writeFile(t, root, "grows.txt", strings.Repeat("c", 3*1024*1024))
	set3, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture (3): %v", err)
	}
	rs3 := set3.Roots["proj-c"]
	out3 := runGitT(t, root, "--git-dir="+gitDir, "ls-tree", "-r", "--name-only", rs3.TreeHash)
	if !strings.Contains(out3, "grows.txt") {
		t.Errorf("previously-tracked file dropped after growing past the ceiling:\n%s", out3)
	}
	_ = rs2
}

func TestCapture_RespectsIncludePaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "inscope/keep.txt", "keep")
	writeFile(t, root, "outofscope/skip.txt", "skip")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-d", Root: root, IncludePaths: []string{"inscope"}}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	rs := set.Roots["proj-d"]
	if rs.Err != nil {
		t.Fatalf("capture error: %v", rs.Err)
	}

	gitDir := sg.gitDirFor("proj-d")
	out := runGitT(t, root, "--git-dir="+gitDir, "ls-tree", "-r", "--name-only", rs.TreeHash)
	if strings.Contains(out, "outofscope") {
		t.Errorf("captured tree includes out-of-scope path:\n%s", out)
	}
	if !strings.Contains(out, "inscope/keep.txt") {
		t.Errorf("captured tree missing in-scope path:\n%s", out)
	}
}

// --- Capture: the two failure modes -----------------------------------------

func TestCapture_LoudInfrastructureFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit test not meaningful on windows")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.MkdirAll(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	baseDir := filepath.Join(locked, "shadow-store") // NewShadowGit must create this and cannot
	_, err := NewShadowGit(baseDir, WithLogger(newLoggerT(t)))
	if err == nil {
		t.Fatal("NewShadowGit: expected error for unwritable base directory, got nil")
	}
	if !errors.Is(err, ErrShadowStoreUnavailable) {
		t.Errorf("NewShadowGit error = %v, want wrapped ErrShadowStoreUnavailable", err)
	}
}

func TestCapture_LoudInfrastructureFailure_MissingGitBinary(t *testing.T) {
	_, err := NewShadowGit(t.TempDir(), WithGitBinary("nanite-snapshot-nonexistent-git-binary"), WithLogger(newLoggerT(t)))
	if err == nil {
		t.Fatal("NewShadowGit: expected error for missing git binary, got nil")
	}
	if !errors.Is(err, ErrShadowStoreUnavailable) {
		t.Errorf("NewShadowGit error = %v, want wrapped ErrShadowStoreUnavailable", err)
	}
}

func TestCapture_SoftPerTargetFailure_NeverBlocksOtherTargets(t *testing.T) {
	sg := newShadowGitT(t)

	healthyRoot := t.TempDir()
	writeFile(t, healthyRoot, "ok.txt", "fine")

	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")

	targets := []Target{
		{ID: "missing", Root: missingRoot},
		{ID: "healthy", Root: healthyRoot},
	}
	set, err := sg.Capture(context.Background(), targets)
	if err != nil {
		t.Fatalf("Capture returned a top-level error for a soft per-target failure: %v", err)
	}

	missing := set.Roots["missing"]
	if missing.Err == nil {
		t.Error("expected RootSnapshot.Err for target with missing Root")
	}

	healthy := set.Roots["healthy"]
	if healthy.Err != nil {
		t.Fatalf("healthy target unexpectedly failed: %v", healthy.Err)
	}
	if healthy.TreeHash == "" {
		t.Error("healthy target has no TreeHash")
	}
}

func TestCapture_InvalidTargetIsSoftFailure(t *testing.T) {
	sg := newShadowGitT(t)
	set, err := sg.Capture(context.Background(), []Target{{ID: "", Root: "/tmp/whatever"}})
	if err != nil {
		t.Fatalf("Capture returned a top-level error for an invalid target: %v", err)
	}
	if len(set.Roots) != 0 {
		// an empty-ID target can't be keyed into Roots meaningfully; make
		// sure it was at least not silently treated as a success.
		for id, rs := range set.Roots {
			if rs.Err == nil {
				t.Errorf("target %q should have recorded an error", id)
			}
		}
	}
}

// --- Diff --------------------------------------------------------------------

func TestDiff_AddedModifiedDeleted(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.txt", "unchanged")
	writeFile(t, root, "modify.txt", "before")
	writeFile(t, root, "remove.txt", "gone soon")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-e", Root: root}
	from, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture (from): %v", err)
	}

	writeFile(t, root, "modify.txt", "after")
	writeFile(t, root, "new.txt", "brand new")
	if err := os.Remove(filepath.Join(root, "remove.txt")); err != nil {
		t.Fatal(err)
	}

	to, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture (to): %v", err)
	}

	diff, err := sg.Diff(context.Background(), from, to)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	rd, ok := diff.Roots["proj-e"]
	if !ok {
		t.Fatal("Diff missing proj-e")
	}
	got := sortedPaths(rd.Files)
	want := []string{"added:new.txt", "deleted:remove.txt", "modified:modify.txt"}
	if !equalStrings(got, want) {
		t.Errorf("Diff.Files = %v, want %v", got, want)
	}
}

func TestDiff_MissingShadowStoreIsHardError(t *testing.T) {
	sg := newShadowGitT(t)
	from := SnapshotSet{Roots: map[string]RootSnapshot{"ghost": {TargetID: "ghost", TreeHash: emptyTreeHash}}}
	to := SnapshotSet{Roots: map[string]RootSnapshot{"ghost": {TargetID: "ghost", TreeHash: emptyTreeHash}}}
	if _, err := sg.Diff(context.Background(), from, to); err == nil {
		t.Fatal("Diff: expected error for a target with no shadow store, got nil")
	}
}

// --- Preview -------------------------------------------------------------

func TestPreview_ReportsAllRequestedKinds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "same.txt", "same content")
	writeFile(t, root, "will-change.txt", "original")
	writeFile(t, root, "will-be-deleted.txt", "present in snapshot")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-f", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Mutate on-disk state after the capture.
	writeFile(t, root, "will-change.txt", "modified on disk")
	if err := os.Remove(filepath.Join(root, "will-be-deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "not-in-snapshot.txt", "new since capture")

	paths := []string{
		JoinPath("proj-f", "same.txt"),
		JoinPath("proj-f", "will-change.txt"),
		JoinPath("proj-f", "will-be-deleted.txt"),
		JoinPath("proj-f", "not-in-snapshot.txt"),
	}
	preview, err := sg.Preview(context.Background(), set, paths)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	rp := preview.Roots["proj-f"]
	got := make(map[string]ChangeKind, len(rp.Changes))
	for _, c := range rp.Changes {
		got[c.Path] = c.Kind
	}
	want := map[string]ChangeKind{
		"same.txt":            ChangeUnchanged,
		"will-change.txt":     ChangeModified,
		"will-be-deleted.txt": ChangeAdded, // restoring re-creates it
		"not-in-snapshot.txt": ChangeDeleted,
	}
	for path, wantKind := range want {
		if got[path] != wantKind {
			t.Errorf("Preview kind for %q = %q, want %q", path, got[path], wantKind)
		}
	}
}

// --- Restore ---------------------------------------------------------------

func TestRestore_SelectiveOnlyTouchesRequestedPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "a-v1")
	writeFile(t, root, "b.txt", "b-v1")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-g", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	writeFile(t, root, "a.txt", "a-v2")
	writeFile(t, root, "b.txt", "b-v2")

	if err := sg.Restore(context.Background(), set, []string{JoinPath("proj-g", "a.txt")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := readFile(t, root, "a.txt"); got != "a-v1" {
		t.Errorf("a.txt = %q, want a-v1 (requested path should have been restored)", got)
	}
	if got := readFile(t, root, "b.txt"); got != "b-v2" {
		t.Errorf("b.txt = %q, want b-v2 (non-requested path must be untouched)", got)
	}
}

func TestRestore_EmptyPathsIsNoOp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "a-v1")
	sg := newShadowGitT(t)
	target := Target{ID: "proj-h", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	writeFile(t, root, "a.txt", "a-v2")
	if err := sg.Restore(context.Background(), set, nil); err != nil {
		t.Fatalf("Restore(nil): %v", err)
	}
	if got := readFile(t, root, "a.txt"); got != "a-v2" {
		t.Errorf("a.txt = %q, want a-v2 (empty paths must restore nothing)", got)
	}
}

func TestRestore_DeletesPathNotPresentInSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.txt", "keep")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-i", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	writeFile(t, root, "new-since-capture.txt", "shouldn't exist after restore")

	if err := sg.Restore(context.Background(), set, []string{JoinPath("proj-i", "new-since-capture.txt")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if fileExists(root, "new-since-capture.txt") {
		t.Error("expected restore to delete a path absent from the snapshot")
	}
}

func TestRestore_SkipsWriteWhenAlreadyMatching_PreservesMtime(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "stable.txt", "unchanging content")

	sg := newShadowGitT(t)
	target := Target{ID: "proj-j", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	full := filepath.Join(root, "stable.txt")
	before, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // ensure any write would produce an observably different mtime

	if err := sg.Restore(context.Background(), set, []string{JoinPath("proj-j", "stable.txt")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	after, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed for a path that already matched the snapshot: before=%v after=%v", before.ModTime(), after.ModTime())
	}
}

func TestRestore_Symlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	sg := newShadowGitT(t)
	target := Target{ID: "proj-k", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := sg.Restore(context.Background(), set, []string{JoinPath("proj-k", "link.txt")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(root, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("restored link.txt is not a symlink")
	}
	dest, err := os.Readlink(filepath.Join(root, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if dest != "real.txt" {
		t.Errorf("symlink target = %q, want real.txt", dest)
	}
}

func TestRestore_UnsafePathRejected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "a")
	sg := newShadowGitT(t)
	target := Target{ID: "proj-l", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	err = sg.Restore(context.Background(), set, []string{JoinPath("proj-l", "../escape.txt")})
	if err == nil {
		t.Fatal("Restore: expected error for a path-traversal restore path, got nil")
	}
}

// --- working-directory / metadata isolation ---------------------------------

func gitIndexSnapshot(t *testing.T, repoDir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoDir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWorkingDirectoryIsolation_RealRepoUntouched(t *testing.T) {
	realRepo := t.TempDir()
	newRealRepo(t, realRepo)
	writeFile(t, realRepo, "src/app.go", "package main")

	headBefore := strings.TrimSpace(runGitT(t, realRepo, "rev-parse", "HEAD"))
	indexBefore := gitIndexSnapshot(t, realRepo)

	sg := newShadowGitT(t)
	target := Target{ID: "isolation-target", Root: realRepo}

	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	rs := set.Roots["isolation-target"]
	if rs.Err != nil {
		t.Fatalf("capture error: %v", rs.Err)
	}

	writeFile(t, realRepo, "src/app.go", "package main // changed")
	if err := sg.Restore(context.Background(), set, []string{JoinPath("isolation-target", "src/app.go")}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	headAfter := strings.TrimSpace(runGitT(t, realRepo, "rev-parse", "HEAD"))
	indexAfter := gitIndexSnapshot(t, realRepo)
	status := runGitT(t, realRepo, "status", "--porcelain")

	if headBefore != headAfter {
		t.Errorf("real repo HEAD changed: before=%s after=%s", headBefore, headAfter)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Error("real repo .git/index bytes changed after shadow capture+restore")
	}
	// The real repo's index should still only reflect its own tracked
	// state (README.md from init); src/app.go was never added to the real
	// repo, so it should show up as untracked, not staged/modified.
	if strings.Contains(status, "M ") || strings.Contains(status, "A ") {
		t.Errorf("real repo status shows staged changes it never made: %q", status)
	}
}

func TestWorkingDirectoryIsolation_EnvLeakage(t *testing.T) {
	realRepo := t.TempDir()
	newRealRepo(t, realRepo)
	headBefore := strings.TrimSpace(runGitT(t, realRepo, "rev-parse", "HEAD"))

	otherRepo := t.TempDir()
	writeFile(t, otherRepo, "a.txt", "a")

	sg := newShadowGitT(t)
	target := Target{ID: "leak-target", Root: otherRepo}

	// Simulate a poisoned environment, as a real git hook sets for its
	// child processes: GIT_DIR/GIT_WORK_TREE pointed at the real repo.
	t.Setenv("GIT_DIR", filepath.Join(realRepo, ".git"))
	t.Setenv("GIT_WORK_TREE", realRepo)

	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		t.Fatalf("Capture under poisoned env: %v", err)
	}
	rs := set.Roots["leak-target"]
	if rs.Err != nil {
		t.Fatalf("capture error under poisoned env: %v", rs.Err)
	}

	gitDir := sg.gitDirFor("leak-target")
	out := runGitT(t, otherRepo, "--git-dir="+gitDir, "ls-tree", "-r", "--name-only", rs.TreeHash)
	if !strings.Contains(out, "a.txt") {
		t.Errorf("capture under poisoned env did not land in the correct shadow store:\n%s", out)
	}

	headAfter := strings.TrimSpace(runGitT(t, realRepo, "rev-parse", "HEAD"))
	if headBefore != headAfter {
		t.Errorf("real repo HEAD changed under poisoned env: before=%s after=%s", headBefore, headAfter)
	}
}

// --- Cleanup -----------------------------------------------------------------

func TestCleanup_MaxSnapshotSets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "v0")
	sg := newShadowGitT(t)
	target := Target{ID: "proj-cleanup", Root: root}

	for i := 0; i < 5; i++ {
		writeFile(t, root, "a.txt", string(rune('a'+i)))
		if _, err := sg.Capture(context.Background(), []Target{target}); err != nil {
			t.Fatalf("Capture %d: %v", i, err)
		}
		time.Sleep(time.Millisecond) // keep ref timestamps distinct
	}

	res, err := sg.Cleanup(context.Background(), "proj-cleanup", CleanupPolicy{MaxSnapshotSets: 2})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if res.Removed != 3 {
		t.Errorf("Removed = %d, want 3", res.Removed)
	}
	if res.RemainingCount != 2 {
		t.Errorf("RemainingCount = %d, want 2", res.RemainingCount)
	}

	gitDir := sg.gitDirFor("proj-cleanup")
	out := runGitT(t, root, "--git-dir="+gitDir, "for-each-ref", "--format=%(refname)", snapshotRefPrefix)
	remaining := len(splitLines([]byte(out)))
	if remaining != 2 {
		t.Errorf("remaining refs on disk = %d, want 2", remaining)
	}
}

func TestCleanup_MaxAge(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "v0")
	sg := newShadowGitT(t)
	target := Target{ID: "proj-cleanup-age", Root: root}

	if _, err := sg.Capture(context.Background(), []Target{target}); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	res, err := sg.Cleanup(context.Background(), "proj-cleanup-age", CleanupPolicy{MaxAge: time.Nanosecond})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1 (everything should be older than 1ns almost immediately)", res.Removed)
	}
}

func TestPurge_RemovesShadowStore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "v0")
	sg := newShadowGitT(t)
	target := Target{ID: "proj-purge", Root: root}
	if _, err := sg.Capture(context.Background(), []Target{target}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := sg.Purge(context.Background(), "proj-purge"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := sg.openShadowRepo("proj-purge"); err == nil {
		t.Error("expected openShadowRepo to fail after Purge")
	}
	// Purge on a target that never had a shadow store is a no-op, not an error.
	if err := sg.Purge(context.Background(), "never-existed"); err != nil {
		t.Errorf("Purge on nonexistent target: %v", err)
	}
}

// --- JoinPath / SplitPath ----------------------------------------------------

func TestJoinSplitPath(t *testing.T) {
	ref := JoinPath("proj-1", "internal/foo.go")
	targetID, rel, err := SplitPath(ref)
	if err != nil {
		t.Fatalf("SplitPath: %v", err)
	}
	if targetID != "proj-1" || rel != "internal/foo.go" {
		t.Errorf("SplitPath = (%q, %q), want (proj-1, internal/foo.go)", targetID, rel)
	}
	if _, _, err := SplitPath("not-a-valid-ref"); err == nil {
		t.Error("SplitPath: expected error for a hand-built string, got nil")
	}
}

// --- NoOpProvider -------------------------------------------------------------

func TestNoOpProvider(t *testing.T) {
	var p FilesystemSnapshotProvider = NoOpProvider{}
	set, err := p.Capture(context.Background(), []Target{{ID: "x", Root: "/tmp"}})
	if err != nil || len(set.Roots) != 0 {
		t.Errorf("Capture = (%v, %v), want empty/nil", set, err)
	}
	if _, err := p.Diff(context.Background(), set, set); err != nil {
		t.Errorf("Diff: %v", err)
	}
	if _, err := p.Preview(context.Background(), set, nil); err != nil {
		t.Errorf("Preview: %v", err)
	}
	if err := p.Restore(context.Background(), set, nil); err != nil {
		t.Errorf("Restore: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
