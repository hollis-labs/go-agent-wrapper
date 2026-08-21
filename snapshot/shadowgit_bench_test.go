package snapshot

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// generateBenchTree builds a realistic-shaped project tree: numFiles small
// source-like files spread across a handful of nested directories, plus a
// gitignored dependency directory the same order of magnitude as a real
// node_modules (proving exclusion doesn't dominate capture cost).
func generateBenchTree(b *testing.B, root string, numFiles, ignoredFiles int) {
	b.Helper()
	dirs := []string{"internal/api", "internal/store", "internal/chat", "ui/src/components", "docs", "cmd/app"}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n*.log\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < numFiles; i++ {
		dir := dirs[i%len(dirs)]
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			b.Fatal(err)
		}
		content := fmt.Sprintf("package p\n\n// file %d\nfunc F%d() int { return %d }\n", i, i, i)
		if err := os.WriteFile(filepath.Join(full, fmt.Sprintf("file_%04d.go", i)), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	nmDir := filepath.Join(root, "node_modules", "some-pkg")
	if err := os.MkdirAll(nmDir, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < ignoredFiles; i++ {
		if err := os.WriteFile(filepath.Join(nmDir, fmt.Sprintf("dep_%04d.js", i)), []byte("module.exports = {};\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

// mutateFewFiles simulates a realistic per-model-step edit: a handful of
// files touched, not the whole tree — matching the "several capture pairs
// per turn" cadence Capture needs to be cheap at.
func mutateFewFiles(b *testing.B, root string, numFiles, seed int) {
	b.Helper()
	dirs := []string{"internal/api", "internal/store", "internal/chat", "ui/src/components", "docs", "cmd/app"}
	r := rand.New(rand.NewSource(int64(seed)))
	for i := 0; i < 3; i++ {
		idx := r.Intn(numFiles)
		dir := dirs[idx%len(dirs)]
		path := filepath.Join(root, dir, fmt.Sprintf("file_%04d.go", idx))
		content := fmt.Sprintf("package p\n\n// file %d, mutation %d\nfunc F%d() int { return %d }\n", idx, seed, idx, idx+seed)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCapture_RealisticTree measures steady-state per-model-step
// Capture latency: a ~2000-file tracked tree plus a ~3000-file gitignored
// node_modules-shaped directory, with only a handful of files actually
// touched between captures (the realistic case — before-the-model-call and
// after-a-cleanly-completed-step captures see small deltas, not a fresh
// tree every time).
func BenchmarkCapture_RealisticTree(b *testing.B) {
	root := b.TempDir()
	const numFiles = 2000
	generateBenchTree(b, root, numFiles, 3000)

	sg, err := NewShadowGit(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	target := Target{ID: "bench-project", Root: root}

	// Prime the shadow store with an initial full capture so steady-state
	// iterations measure incremental cost, not first-ever-capture cost.
	if _, err := sg.Capture(context.Background(), []Target{target}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mutateFewFiles(b, root, numFiles, i)
		set, err := sg.Capture(context.Background(), []Target{target})
		if err != nil {
			b.Fatal(err)
		}
		if rs := set.Roots["bench-project"]; rs.Err != nil {
			b.Fatal(rs.Err)
		}
	}
}

// BenchmarkCapture_FirstCapture measures the one-time cost of the very
// first capture of a tree this size (no prior shadow-repo state to diff
// against), as a reference point against the steady-state number above.
func BenchmarkCapture_FirstCapture(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		root := b.TempDir()
		generateBenchTree(b, root, 2000, 3000)
		sg, err := NewShadowGit(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		target := Target{ID: fmt.Sprintf("bench-first-%d", i), Root: root}
		b.StartTimer()

		if _, err := sg.Capture(context.Background(), []Target{target}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPreview_SinglePath and BenchmarkRestore_SinglePath measure the
// per-path cost of the other two hot-ish operations at the same tree size,
// since both are plausible per-file-click UI operations.
func BenchmarkPreview_SinglePath(b *testing.B) {
	root := b.TempDir()
	generateBenchTree(b, root, 2000, 3000)
	sg, err := NewShadowGit(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	target := Target{ID: "bench-preview", Root: root}
	set, err := sg.Capture(context.Background(), []Target{target})
	if err != nil {
		b.Fatal(err)
	}
	paths := []string{JoinPath("bench-preview", "internal/api/file_0001.go")}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sg.Preview(context.Background(), set, paths); err != nil {
			b.Fatal(err)
		}
	}
}
