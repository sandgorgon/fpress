package par

import (
	"sync/atomic"
	"testing"
)

func TestForVisitsEveryIndexExactlyOnce(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 100, 10007} {
		for _, workers := range []int{0, 1, 2, 3, 16, 1000} {
			for _, chunk := range []int{0, 1, 5, 64, 100000} {
				seen := make([]int32, n)
				For(n, workers, chunk, func(i int) { atomic.AddInt32(&seen[i], 1) })
				for i, c := range seen {
					if c != 1 {
						t.Fatalf("n=%d workers=%d chunk=%d: index %d visited %d times", n, workers, chunk, i, c)
					}
				}
			}
		}
	}
}

func TestForWorkerIndexIsInRange(t *testing.T) {
	var bad atomic.Int32
	ForWorker(5000, 7, 3, func(w, i int) {
		if w < 0 || w >= 7 {
			bad.Add(1)
		}
	})
	if bad.Load() != 0 {
		t.Fatal("worker index out of range")
	}
}

func TestSingleWorkerRunsInOrder(t *testing.T) {
	var got []int
	For(50, 1, 4, func(i int) { got = append(got, i) })
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order at %d: %d", i, v)
		}
	}
}

func TestDoRunsAll(t *testing.T) {
	var a, b, c atomic.Int32
	Do(2, func() { a.Add(1) }, func() { b.Add(1) }, func() { c.Add(1) })
	if a.Load() != 1 || b.Load() != 1 || c.Load() != 1 {
		t.Fatal("not all functions ran")
	}
}
