package prep

import (
	"bytes"
	"compress/flate"
	"sort"

	"fpress/matrix"
)

// DefaultWidth returns the fold width used when nothing better is known: the
// largest power of two not exceeding sqrt(n), at least 8.
func DefaultWidth(n int) int {
	root := 1
	for (root+1)*(root+1) <= n {
		root++
	}
	w := 8
	for w*2 <= root {
		w *= 2
	}
	return w
}

// DetectWidths looks for row lengths at which the data repeats itself. For
// each lag it takes the difference between every byte and the byte one lag
// earlier, and scores the lag by how much of the data shares the single most
// common difference. That is high both for exact repeats (difference 0) and
// for "same as before plus a constant" (a gradient). It returns up to k
// distinct strong peaks, best first, skipping multiples of a period already
// chosen. Random data gives nothing, and only the front of large inputs is
// sampled. The result is a list of hypotheses; Search tests them.
func DetectWidths(data []byte, k int) []int {
	const (
		maxSample = 16 << 10
		maxLag    = 1024
		minLag    = 4
	)
	if len(data) > maxSample {
		data = data[:maxSample]
	}
	hi := min(maxLag, len(data)/2)
	if hi <= minLag+2 || k <= 0 {
		return nil
	}
	score := make([]float64, hi+2)
	for lag := minLag; lag <= hi; lag++ {
		var hist [256]int32
		var top int32
		a, b := data[:len(data)-lag], data[lag:]
		for i := range a {
			d := b[i] - a[i]
			if hist[d]++; hist[d] > top {
				top = hist[d]
			}
		}
		score[lag] = float64(top) / float64(len(a))
	}
	sorted := append([]float64(nil), score[minLag:hi+1]...)
	sort.Float64s(sorted)
	threshold := max(0.05, sorted[len(sorted)/2]+0.02) // above the typical lag

	var peaks []int
	for lag := minLag; lag <= hi; lag++ {
		if score[lag] >= threshold && score[lag] > score[lag-1] && score[lag] >= score[lag+1] {
			peaks = append(peaks, lag)
		}
	}
	sort.SliceStable(peaks, func(i, j int) bool { return score[peaks[i]] > score[peaks[j]] })
	var picked []int
next:
	for _, lag := range peaks {
		for _, p := range picked {
			if lag%p == 0 {
				continue next
			}
		}
		picked = append(picked, lag)
		if len(picked) == k {
			break
		}
	}
	return picked
}

// Catalogue is the set of chains Search tries at every width.
var Catalogue = []Chain{
	nil,
	{{Kind: DeltaUpSub}},
	{{Kind: DeltaUpXor}},
	{{Kind: DeltaLeftSub}},
	{{Kind: DeltaLeftXor}},
	{{Kind: DeltaLeftSub}, {Kind: DeltaUpSub}},
	{{Kind: BitPlanes}},
	{{Kind: BitPlanes}, {Kind: DeltaUpXor}},
	{{Kind: Deinterleave, K: 2}},
	{{Kind: Deinterleave, K: 2}, {Kind: DeltaLeftSub}},
	{{Kind: Deinterleave, K: 4}},
	{{Kind: Deinterleave, K: 2}, {Kind: DeltaUpSub}},
}

// Candidate is a fold width and chain with its trial cost.
type Candidate struct {
	Width int
	Chain Chain
	Cost  int // bytes of DEFLATE output on the sample; smaller is better
}

// SearchOptions tunes Search.
type SearchOptions struct {
	// Width fixes the fold width; 0 tries the default width and detected periods.
	Width int
	// MaxSample bounds how much of the data the trials use (0 = 64 KiB).
	MaxSample int
	// FrameRows, when positive, says the data is raw video whose frames are
	// FrameRows rows each at the trial width. Chains that difference against the
	// previous frame are then tried too, and the sample is widened to hold a few
	// frames (the 64 KiB default would not even reach the second one).
	FrameRows int
}

// frameChains are the extra chains tried for video of k rows per frame.
func frameChains(k int) []Chain {
	return []Chain{
		{{Kind: DeltaFrameSub, K: k}},
		{{Kind: DeltaFrameXor, K: k}},
		{{Kind: DeltaFrameSub, K: k}, {Kind: DeltaLeftSub}},
	}
}

// Search ranks (width, chain) pairs by how well the prepared bytes deflate.
// DEFLATE is only a stand-in for "has exploitable repetition": it is fast and
// ranks arrangements sensibly, but the caller makes the final size decision.
// For large inputs the trials run on a sample of the front. The result is
// never empty for non-empty data and is sorted best first; ties prefer the
// shorter chain, then the narrower width, so the order is deterministic.
func Search(data []byte, opt SearchOptions) []Candidate {
	if len(data) == 0 {
		return nil
	}
	maxSample := opt.MaxSample
	if maxSample <= 0 {
		maxSample = 64 << 10
	}
	frameBytes := 0
	if opt.FrameRows > 0 && opt.Width > 0 {
		frameBytes = opt.FrameRows * opt.Width
		maxSample = max(maxSample, 3*frameBytes) // a few frames, so the delta is visible
	}
	sample := data[:min(len(data), maxSample)]

	var widths []int
	add := func(w int) {
		if w < 1 || w > len(sample) {
			return
		}
		for _, have := range widths {
			if have == w {
				return
			}
		}
		widths = append(widths, w)
	}
	if opt.Width > 0 {
		add(opt.Width)
	} else {
		add(DefaultWidth(len(data)))
		for _, w := range DetectWidths(sample, 3) {
			add(w)
		}
	}
	if len(widths) == 0 { // fixed width wider than the sample
		widths = []int{min(max(opt.Width, 1), len(data))}
		sample = data
	}

	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.BestSpeed)
	var out []Candidate
	for _, w := range widths {
		folded := matrix.FromBytes(sample, w)
		chains := Catalogue
		if frameBytes > 0 {
			chains = append(append([]Chain(nil), Catalogue...), frameChains(opt.FrameRows)...)
		}
		for _, chain := range chains {
			prepared, err := chain.Forward(folded)
			if err != nil {
				continue // chain does not fit this width
			}
			buf.Reset()
			fw.Reset(&buf)
			fw.Write(prepared.Pix())
			fw.Close()
			out = append(out, Candidate{Width: w, Chain: chain, Cost: buf.Len()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Cost != b.Cost {
			return a.Cost < b.Cost
		}
		if len(a.Chain) != len(b.Chain) {
			return len(a.Chain) < len(b.Chain)
		}
		return a.Width < b.Width
	})
	return out
}
