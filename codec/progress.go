package codec

import (
	"sync"
	"sync/atomic"
)

// Progress lets another goroutine follow a running Compress, CompressReport or
// CompressStream: set Options.Progress to a new Progress and call Status while
// the call runs. A Progress follows one call at a time.
//
// The estimate comes from the work that takes the longest. Inside one piece of
// the input (the whole input, or one super-segment of a very large one) that is
// the context-mixing coder: Status counts the bytes its whole-input attempts
// have coded, or, when there are none, the segments finished. When neither
// runs (-no-cm, or the fastest preset on a small input), only whole pieces are
// counted, and Measured is false.
type Progress struct {
	mu       sync.Mutex
	total    int64    // input size
	base     int64    // bytes in pieces already finished
	piece    int64    // bytes in the current piece
	whole    []*meter // whole-piece context-mixing attempts
	seg      *meter   // segments finished
	checking bool
	best     int64 // highest Done reported, so Done never goes back
}

// ProgressStatus is a snapshot of a Progress.
type ProgressStatus struct {
	Done, Total int64 // estimated input bytes finished, of the input size
	Measured    bool  // Done includes progress inside the current piece
	Checking    bool  // the compressor is decoding its output to check it
}

type meter struct {
	done  atomic.Int64
	total int64
}

func (m *meter) fraction() float64 {
	return min(float64(m.done.Load())/float64(max(m.total, 1)), 1)
}

// Status returns the current estimate. It is safe to call from any goroutine.
func (p *Progress) Status() ProgressStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := ProgressStatus{Total: p.total, Checking: p.checking}
	frac := 0.0
	switch {
	case p.checking:
		frac, s.Measured = 1, true
	case len(p.whole) > 0:
		// Attempts may run side by side or one after another; their combined
		// count moves steadily either way.
		var done, total float64
		for _, m := range p.whole {
			done += m.fraction() * float64(m.total)
			total += float64(m.total)
		}
		frac, s.Measured = done/max(total, 1), true
	case p.seg != nil:
		frac, s.Measured = p.seg.fraction(), true
	}
	s.Done = p.base + int64(frac*float64(p.piece))
	p.best = max(p.best, s.Done)
	s.Done = p.best
	s.Measured = s.Measured || s.Done >= s.Total
	return s
}

// The methods below are called by the compressor; all accept a nil Progress.

// start begins a call on an input of total bytes.
func (p *Progress) start(total int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.total, p.base, p.best = total, 0, 0
	p.piece, p.whole, p.seg, p.checking = 0, nil, nil, false
	p.mu.Unlock()
}

// beginPiece starts a piece of n bytes after everything before it finished.
func (p *Progress) beginPiece(n int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.piece, p.whole, p.seg, p.checking = n, nil, nil, false
	p.mu.Unlock()
}

// endPiece marks the current piece finished.
func (p *Progress) endPiece() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.base += p.piece
	p.piece, p.whole, p.seg, p.checking = 0, nil, nil, false
	p.mu.Unlock()
}

func (p *Progress) setChecking() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.checking = true
	p.mu.Unlock()
}

// wholeMeter registers a whole-piece context-mixing attempt over n bytes. It
// returns nil for a nil Progress.
func (p *Progress) wholeMeter(n int) *atomic.Int64 {
	if p == nil {
		return nil
	}
	m := &meter{total: int64(n)}
	p.mu.Lock()
	p.whole = append(p.whole, m)
	p.mu.Unlock()
	return &m.done
}

// segMeter registers the segmented pass over n bytes.
func (p *Progress) segMeter(n int) *atomic.Int64 {
	if p == nil {
		return nil
	}
	m := &meter{total: int64(n)}
	p.mu.Lock()
	p.seg = m
	p.mu.Unlock()
	return &m.done
}
