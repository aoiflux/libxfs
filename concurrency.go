package libxfs

import (
	"context"
	"runtime"
	"sync"
)

// Optional parallelism.
//
// # What is parallelised, and what is not
//
// Walking a directory tree is inherently sequential: a directory must be read
// before its children are known. Analysing an inode once it has been found is
// not — fragmentation, extended attributes, checksums and directory artifacts
// for one inode are independent of every other inode.
//
// So work is split in two: discovery stays sequential and produces an ordered
// list of inodes, then analysis of that list runs across a bounded pool. Each
// task writes only to its own slot in caller-owned storage, so there is no
// shared mutable state to guard and no lock on the hot path.
//
// # Determinism
//
// Output does not depend on worker count or scheduling. Results are stored by
// index rather than appended as they complete, so a report generated with
// sixteen workers is byte-for-byte identical to one generated with one.
//
// # Degradation
//
// Workers <= 1 runs everything on the calling goroutine with no goroutines
// started at all, which is the default. Parallelism is opt-in.

// Concurrency configures optional parallel execution.
//
// The zero value is sequential.
type Concurrency struct {
	// Workers is the maximum number of tasks executed in parallel. Zero or one
	// runs sequentially on the calling goroutine. Negative means "one per
	// available CPU".
	Workers int
}

// maxAutomaticWorkers caps the pool chosen by Workers < 0, so that a machine
// with many cores does not spawn an unreasonable number of readers against one
// backing file.
const maxAutomaticWorkers = 16

// workerCount resolves the configured setting to an actual pool size, never
// exceeding the amount of work available.
func (c Concurrency) workerCount(taskCount int) int {
	if taskCount <= 1 {
		return 1
	}

	workers := c.Workers
	if workers < 0 {
		workers = runtime.NumCPU()
		if workers > maxAutomaticWorkers {
			workers = maxAutomaticWorkers
		}
	}
	if workers <= 1 {
		return 1
	}
	if workers > taskCount {
		workers = taskCount
	}
	return workers
}

// runBounded applies work to every index in [0, taskCount) using at most the
// configured number of goroutines.
//
// work must confine its writes to storage owned by its own index. The first
// error returned by any task is reported, and the context passed to remaining
// tasks is cancelled so they can stop early; tasks already running are always
// waited for, so no goroutine outlives this call.
func runBounded(ctx context.Context, concurrency Concurrency, taskCount int,
	work func(ctx context.Context, index int) error) error {

	if taskCount <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	workers := concurrency.workerCount(taskCount)
	if workers == 1 {
		// Sequential path: no goroutines, no channels, no cancellation
		// plumbing beyond the caller's own context.
		for index := 0; index < taskCount; index++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := work(ctx, index); err != nil {
				return err
			}
		}
		return nil
	}

	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	indexes := make(chan int)
	var group sync.WaitGroup

	var firstErrorOnce sync.Once
	var firstError error

	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range indexes {
				if err := work(taskCtx, index); err != nil {
					firstErrorOnce.Do(func() {
						firstError = err
						cancel()
					})
					return
				}
			}
		}()
	}

	// The feeder stops early when a task fails or the caller cancels, so no
	// goroutine is left blocked on a send.
feed:
	for index := 0; index < taskCount; index++ {
		select {
		case indexes <- index:
		case <-taskCtx.Done():
			break feed
		}
	}
	close(indexes)
	group.Wait()

	if firstError != nil {
		return firstError
	}
	// Distinguish "the caller cancelled" from "everything completed".
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
