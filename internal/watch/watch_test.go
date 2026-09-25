package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func collect(t *testing.T, dir string, every time.Duration) (func() []string, context.CancelFunc) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	ctx, cancel := context.WithCancel(context.Background())
	go Run(ctx, dir, every, func(rel string) {
		mu.Lock()
		seen = append(seen, rel)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}, cancel
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// The point of a drop box: what was already there is not "dropped", what
// arrives is reported once, and dotfiles are nobody's business.
func TestReportsNewFilesOnceAndIgnoresBaseline(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "old.md"), []byte("baseline"), 0o644)
	seen, cancel := collect(t, dir, 30*time.Millisecond)
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	os.WriteFile(filepath.Join(dir, "new.md"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("noise"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "deep.md"), []byte("hi"), 0o644)

	waitFor(t, func() bool { return len(seen()) >= 2 })
	time.Sleep(120 * time.Millisecond) // long enough for a duplicate to show up if it were going to
	got := seen()
	if len(got) != 2 {
		t.Fatalf("expected exactly new.md and sub/deep.md once each, got %v", got)
	}
	for _, g := range got {
		if g != "new.md" && g != filepath.Join("sub", "deep.md") {
			t.Fatalf("unexpected report %q", g)
		}
	}
}

// A file being copied in grows over several polls. It must be reported only
// after it stops changing, and only once.
func TestWaitsUntilAFileHoldsStill(t *testing.T) {
	dir := t.TempDir()
	seen, cancel := collect(t, dir, 30*time.Millisecond)
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	// Keep writing faster than the poll interval, the way a copy does, so the
	// file never holds still until we stop.
	p := filepath.Join(dir, "big.csv")
	f, _ := os.Create(p)
	for i := 0; i < 40; i++ {
		f.WriteString("row\n")
		time.Sleep(5 * time.Millisecond)
		if len(seen()) != 0 {
			t.Fatal("reported while the file was still being written")
		}
	}
	f.Close()
	waitFor(t, func() bool { return len(seen()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := seen(); len(got) != 1 || got[0] != "big.csv" {
		t.Fatalf("expected big.csv exactly once, got %v", got)
	}
}

// Editing a file that was already reported reports it again — a corrected
// note is worth a second look — but a deleted one is simply forgotten.
func TestChangedFileIsReportedAgain(t *testing.T) {
	dir := t.TempDir()
	seen, cancel := collect(t, dir, 30*time.Millisecond)
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	p := filepath.Join(dir, "note.md")
	os.WriteFile(p, []byte("v1"), 0o644)
	waitFor(t, func() bool { return len(seen()) == 1 })
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(p, []byte("v2, longer"), 0o644)
	waitFor(t, func() bool { return len(seen()) == 2 })
	os.Remove(p)
	time.Sleep(120 * time.Millisecond)
	if len(seen()) != 2 {
		t.Fatalf("a removed file must not be reported, got %v", seen())
	}
}
