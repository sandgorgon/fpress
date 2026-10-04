// Package par runs loops across goroutines in a way that cannot change
// results: work is handed out dynamically, but every iteration writes only to
// its own slot, so the outcome never depends on scheduling.
package par

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// Workers returns n, or GOMAXPROCS when n <= 0.
func Workers(n int) int {
	if n <= 0 {
		return runtime.GOMAXPROCS(0)
	}
	return n
}

// For calls body(i) for every i in [0, n) using up to workers goroutines, and
// returns when all calls have finished. Iterations are handed out in chunks of
// the given size (at least 1). With one worker, or too little work to share,
// it runs inline in order.
func For(n, workers, chunk int, body func(i int)) {
	ForWorker(n, workers, chunk, func(_, i int) { body(i) })
}

// ForWorker is For with the index of the worker goroutine (0 <= w < workers)
// passed along, so each worker can keep its own scratch space.
func ForWorker(n, workers, chunk int, body func(w, i int)) {
	if n <= 0 {
		return
	}
	chunk = max(chunk, 1)
	workers = min(Workers(workers), (n+chunk-1)/chunk)
	if workers <= 1 {
		for i := 0; i < n; i++ {
			body(0, i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				lo := int(next.Add(int64(chunk))) - chunk
				if lo >= n {
					return
				}
				for i := lo; i < min(lo+chunk, n); i++ {
					body(w, i)
				}
			}
		}(w)
	}
	wg.Wait()
}

// Do runs the given functions concurrently (up to workers at a time) and
// waits for all of them.
func Do(workers int, fns ...func()) {
	For(len(fns), workers, 1, func(i int) { fns[i]() })
}
