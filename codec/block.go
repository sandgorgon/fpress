package codec

import (
	"bytes"
	"compress/flate"
	"math"

	"fpress/fractal"
	"fpress/internal/par"
	"fpress/prep"
)

// blockInfo records what encodeBlock tried. Sizes are payload bytes (no
// container header); -1 means the candidate was not tried.
type blockInfo struct {
	rejected bool // quick reject: judged incompressible, stored without trying anything

	stored, flate, prepFlate, fractal, cm int
	cmWidth                               int
	cmChain                               prep.Chain

	prepWidth    int
	prepChain    prep.Chain
	fractalWidth int
	fractalChain prep.Chain
	fractalStats *fractal.Stats
	fractalTiles fractal.TileStats
	fractalProbe fractal.ProbeResult
	fractalGated bool // every fractal attempt was skipped by the gate
}

// encodeBlock returns the smallest encoding of data among stored, flate,
// prep+flate and fractal, as a mode and the payload for that mode.
func encodeBlock(data []byte, opt Options) (Mode, []byte, blockInfo, error) {
	info := blockInfo{stored: len(data), flate: -1, prepFlate: -1, fractal: -1, cm: -1}
	bestMode, best := ModeStored, data
	consider := func(m Mode, p []byte) {
		if len(p) < len(best) {
			bestMode, best = m, p
		}
	}
	if len(data) == 0 {
		return bestMode, best, info, nil
	}

	if !opt.NoQuickReject && hopeless(data) {
		info.rejected = true
		return bestMode, best, info, nil
	}

	level := flate.BestCompression
	if opt.FastFlate {
		level = flate.DefaultCompression
	}

	// The candidate encodings are independent of each other, so they run side
	// by side (unless the caller is already running blocks in parallel and has
	// set Fractal.Workers to 1). Results go to fixed slots and are compared
	// afterwards in a fixed order, so the outcome never depends on which
	// finishes first.
	workers := par.Workers(opt.Fractal.Workers)
	var fl []byte
	var cands []prep.Candidate
	if len(data) >= minPrepSize {
		par.Do(workers,
			func() { fl = deflateAt(data, level) },
			func() { cands = candidates(data, opt) })
	} else {
		fl = deflateAt(data, level)
	}
	info.flate = len(fl)
	consider(ModeFlate, fl)
	if len(data) < minPrepSize {
		return bestMode, best, info, nil
	}

	type result struct {
		payload []byte
		stats   fractal.Stats
		tiles   fractal.TileStats
		probe   fractal.ProbeResult
		skipped bool
		err     error
	}
	var prepRes result
	var fractalAtt, cmAtt []prep.Candidate
	var fractalRes, cmRes []result
	var tasks []func()

	if !opt.DisablePrep && len(cands) > 0 {
		c := cands[0]
		tasks = append(tasks, func() { prepRes.payload, prepRes.err = prepFlatePayload(data, c.Width, c.Chain, level) })
	}
	if !opt.DisableFractal && (opt.MaxFractalSize == 0 || len(data) <= opt.MaxFractalSize) {
		fractalAtt = fractalAttempts(cands, len(data))
		fractalRes = make([]result, len(fractalAtt))
		for i, c := range fractalAtt {
			tasks = append(tasks, func() {
				r := &fractalRes[i]
				r.payload, r.stats, r.tiles, r.probe, r.skipped, r.err = fractalPayload(data, c, opt.Fractal, !opt.NoFractalGate)
			})
		}
	}
	maxCM := opt.MaxCMSize
	if maxCM == 0 {
		maxCM = defaultMaxCM
	}
	if !opt.DisableCM && len(data) <= maxCM {
		cmAtt = cmAttempts(cands, len(data), opt.CMAttempts)
		cmRes = make([]result, len(cmAtt))
		for i, c := range cmAtt {
			tasks = append(tasks, func() { cmRes[i].payload, cmRes[i].err = cmPayload(data, c.Width, c.Chain) })
		}
	}
	par.Do(workers, tasks...)

	// Collect in the order the attempts would have run one after another.
	if prepRes.err != nil {
		return 0, nil, info, prepRes.err
	}
	if prepRes.payload != nil {
		c := cands[0]
		info.prepFlate, info.prepWidth, info.prepChain = len(prepRes.payload), c.Width, c.Chain
		consider(ModePrep, prepRes.payload)
	}
	gated := len(fractalRes) > 0
	for i, r := range fractalRes {
		if r.err != nil {
			return 0, nil, info, r.err
		}
		if r.skipped {
			if info.fractalProbe.Nodes == 0 || r.probe.Hits > info.fractalProbe.Hits {
				info.fractalProbe = r.probe
			}
			continue
		}
		gated = false
		if info.fractal < 0 || len(r.payload) < info.fractal {
			st, ts := r.stats, r.tiles
			info.fractal, info.fractalWidth, info.fractalChain = len(r.payload), fractalAtt[i].Width, fractalAtt[i].Chain
			info.fractalStats, info.fractalTiles = &st, ts
			info.fractalProbe = r.probe
		}
		consider(ModeFractal, r.payload)
	}
	info.fractalGated = gated
	for i, r := range cmRes {
		if r.err != nil {
			return 0, nil, info, r.err
		}
		if info.cm < 0 || len(r.payload) < info.cm {
			info.cm, info.cmWidth, info.cmChain = len(r.payload), cmAtt[i].Width, cmAtt[i].Chain
		}
		consider(ModeCM, r.payload)
	}
	return bestMode, best, info, nil
}

// hopeless is the quick reject: it recognises data none of the modes can
// shrink, so the expensive searches are skipped. It is a heuristic and can only
// cost compression, never correctness, since stored is always valid. Data is
// judged hopeless only if all of these hold:
//
//   - its byte distribution is nearly uniform (order-0 entropy >= 7.9 bits),
//   - a fast DEFLATE pass saves under 1%,
//   - no repeating period shows up in the front of the data (a periodic
//     pattern can be invisible to DEFLATE when the period exceeds its window,
//     but the prep and fractal stages can still use it).
//
// Small inputs are never rejected: trying everything on them is cheap.
func hopeless(data []byte) bool {
	if len(data) < 4096 {
		return false
	}
	var count [256]int
	for _, b := range data {
		count[b]++
	}
	n := float64(len(data))
	entropy := 0.0
	for _, c := range count {
		if c > 0 {
			p := float64(c) / n
			entropy -= p * math.Log2(p)
		}
	}
	if entropy < 7.9 {
		return false
	}
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.BestSpeed)
	fw.Write(data)
	fw.Close()
	if float64(buf.Len()) < 0.99*n {
		return false
	}
	return len(prep.DetectWidths(data, 1)) == 0
}

// cmAttempts picks which (width, chain) hypotheses get the context-mixing
// coder, which is slow: the best candidate from the DEFLATE ranking, a plain
// byte stream (width 0), and the best candidate with no chain (its width gives
// the coder rows to look up, without any transform). Large inputs get fewer.
func cmAttempts(cands []prep.Candidate, dataLen, maxAttempts int) []prep.Candidate {
	var out []prep.Candidate
	if len(cands) > 0 {
		out = append(out, cands[0])
	}
	out = append(out, prep.Candidate{}) // plain stream
	if len(cands) > 0 {
		for _, c := range cands[1:] {
			if len(c.Chain) == 0 && !(len(cands[0].Chain) == 0 && c.Width == cands[0].Width) {
				out = append(out, c)
				break
			}
		}
	}
	limit := 3
	switch {
	case dataLen > 4<<20:
		limit = 1
	case dataLen > 1<<20:
		limit = 2
	}
	if maxAttempts > 0 {
		limit = maxAttempts
	}
	return out[:min(limit, len(out))]
}
