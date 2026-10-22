package codec

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// progressInput is text-like data, long enough to be segmented, so both the
// whole-input attempts and the segmented pass report progress.
func progressInput() []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < 160<<10; i++ {
		b.WriteString("the quick brown fox ")
		b.WriteByte(byte('a' + i%26))
		b.WriteString(" jumps over the lazy dog\n")
	}
	return b.Bytes()
}

// sample reads p every millisecond until stop is closed and returns what it saw.
func sample(p *Progress, stop chan struct{}) <-chan []ProgressStatus {
	out := make(chan []ProgressStatus, 1)
	go func() {
		var seen []ProgressStatus
		for {
			select {
			case <-stop:
				out <- seen
				return
			case <-time.After(time.Millisecond):
				seen = append(seen, p.Status())
			}
		}
	}()
	return out
}

func checkProgress(t *testing.T, seen []ProgressStatus, final ProgressStatus, total int64) {
	t.Helper()
	for i, s := range seen {
		if s.Total != total || s.Done < 0 || s.Done > total {
			t.Fatalf("sample %d: %+v, total %d", i, s, total)
		}
		if i > 0 && s.Done < seen[i-1].Done {
			t.Fatalf("progress went back: %+v after %+v", s, seen[i-1])
		}
	}
	if final.Done != total || !final.Measured {
		t.Fatalf("final status %+v, want all %d bytes done", final, total)
	}
}

func TestProgressDoesNotChangeOutput(t *testing.T) {
	data := progressInput()
	for _, preset := range []Preset{PresetDefault, PresetFast, PresetFastest} {
		opt := PresetOptions(preset)
		want, err := Compress(data, opt)
		if err != nil {
			t.Fatal(err)
		}
		opt.Progress = new(Progress)
		stop := make(chan struct{})
		seen := sample(opt.Progress, stop)
		got, err := Compress(data, opt)
		close(stop)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%v: output changed when progress was followed", preset)
		}
		checkProgress(t, <-seen, opt.Progress.Status(), int64(len(data)))
	}
}

func TestProgressAcrossSuperSegments(t *testing.T) {
	data := progressInput()
	opt := PresetOptions(PresetFast)
	opt.BigSegment = 64 << 10
	opt.Progress = new(Progress)
	stop := make(chan struct{})
	seen := sample(opt.Progress, stop)
	var out bytes.Buffer
	rep, err := CompressStream(&out, bytes.NewReader(data), int64(len(data)), opt)
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BigSegments < 2 {
		t.Fatalf("%d super-segments, want several", rep.BigSegments)
	}
	checkProgress(t, <-seen, opt.Progress.Status(), int64(len(data)))
}

// Status must be safe to call while the compressor updates it (run with -race).
func TestProgressConcurrentStatus(t *testing.T) {
	p := new(Progress)
	p.start(100)
	p.beginPiece(100)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := p.wholeMeter(100)
			for range 100 {
				c.Add(1)
				p.Status()
			}
		}()
	}
	wg.Wait()
	if s := p.Status(); s.Done != 100 {
		t.Fatalf("Done = %d after all attempts finished, want 100", s.Done)
	}
}

// Separate calls share nothing that changes results (the README promises
// that goroutines can compress at the same time; run with -race).
func TestConcurrentCalls(t *testing.T) {
	data := progressInput()
	want, err := Compress(data, PresetOptions(PresetFast))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Compress(data, PresetOptions(PresetFast))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("concurrent Compress: %v, equal %v", err, bytes.Equal(got, want))
			}
			if back, err := Decompress(got); err != nil || !bytes.Equal(back, data) {
				t.Errorf("concurrent Decompress: %v", err)
			}
		}()
	}
	wg.Wait()
}
