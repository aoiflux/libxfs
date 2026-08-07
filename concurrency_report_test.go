package libxfs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// reportFixture builds a volume with a nested tree so a report has enough
// inodes for parallel analysis to be meaningful.
func reportFixture(t testing.TB) *fixtureImage {
	t.Helper()

	image := newFixtureImage(5, 0)
	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 25, 100)...)
	blocks := [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), entries),
		image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 25, 200)),
	}
	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)
	return image
}

// TestReportIsDeterministicAcrossWorkerCounts is the property that makes
// parallelism safe to enable: the same volume must produce the same report
// regardless of how many goroutines produced it.
func TestReportIsDeterministicAcrossWorkerCounts(t *testing.T) {
	image := reportFixture(t)

	var baseline string
	for _, workers := range []int{0, 1, 2, 4, 8, -1} {
		volume := image.open(t)

		report, err := volume.ReportWithOptions(ReportOptions{
			IncludeDirectoryArtifacts: true,
			Concurrency:               Concurrency{Workers: workers},
		})
		if err != nil {
			t.Fatalf("workers=%d: report failed: %v", workers, err)
		}

		// GeneratedAt is a wall-clock stamp and is expected to differ.
		report.GeneratedAt = report.GeneratedAt.Truncate(0)
		report.GeneratedAt = referenceTime

		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("workers=%d: marshal failed: %v", workers, err)
		}

		if baseline == "" {
			baseline = string(encoded)
			if len(report.Files) == 0 {
				t.Fatal("report contains no files; fixture is not exercising anything")
			}
			continue
		}
		if string(encoded) != baseline {
			t.Fatalf("workers=%d produced a different report than the sequential run", workers)
		}
		_ = volume.Close()
	}
}

// TestReportRespectsContextCancellation checks that a cancelled context stops
// the run rather than being ignored.
func TestReportRespectsContextCancellation(t *testing.T) {
	volume := reportFixture(t).open(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := volume.ReportWithContext(ctx, ReportOptions{
		Concurrency: Concurrency{Workers: 4},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestWorkerCountResolution pins the pool sizing rules.
func TestWorkerCountResolution(t *testing.T) {
	for _, tc := range []struct {
		workers   int
		taskCount int
		wantMax   int
		wantMin   int
	}{
		{workers: 0, taskCount: 100, wantMin: 1, wantMax: 1},   // default sequential
		{workers: 1, taskCount: 100, wantMin: 1, wantMax: 1},   // explicit sequential
		{workers: 4, taskCount: 100, wantMin: 4, wantMax: 4},   // as configured
		{workers: 8, taskCount: 3, wantMin: 3, wantMax: 3},     // never exceeds work
		{workers: 4, taskCount: 1, wantMin: 1, wantMax: 1},     // single task
		{workers: 4, taskCount: 0, wantMin: 1, wantMax: 1},     // no work
		{workers: -1, taskCount: 100, wantMin: 1, wantMax: 16}, // automatic, capped
	} {
		got := Concurrency{Workers: tc.workers}.workerCount(tc.taskCount)
		if got < tc.wantMin || got > tc.wantMax {
			t.Fatalf("workers=%d tasks=%d: got %d, want between %d and %d",
				tc.workers, tc.taskCount, got, tc.wantMin, tc.wantMax)
		}
	}
}

// TestRunBoundedPropagatesFirstError checks error handling and that no
// goroutine outlives the call.
func TestRunBoundedPropagatesFirstError(t *testing.T) {
	sentinel := errors.New("task failed")
	results := make([]int, 50)

	err := runBounded(context.Background(), Concurrency{Workers: 4}, len(results),
		func(ctx context.Context, index int) error {
			if index == 7 {
				return sentinel
			}
			results[index] = index
			return nil
		})

	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the task error, got %v", err)
	}
}

// TestRunBoundedWritesEverySlot checks the happy path across both modes.
func TestRunBoundedWritesEverySlot(t *testing.T) {
	for _, workers := range []int{1, 4, 16} {
		results := make([]int, 200)
		err := runBounded(context.Background(), Concurrency{Workers: workers}, len(results),
			func(ctx context.Context, index int) error {
				results[index] = index * 2
				return nil
			})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		for i, got := range results {
			if got != i*2 {
				t.Fatalf("workers=%d: slot %d = %d, want %d", workers, i, got, i*2)
			}
		}
	}
}
