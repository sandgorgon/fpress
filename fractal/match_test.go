package fractal

import (
	"bytes"
	"math/rand"
	"testing"

	"fpress/matrix"
	"fpress/transform"
)

// bruteExists reports whether some domain block of the given scale (2: the
// decimated 2s x 2s domain, 1: an s x s copy), under some isometry and an add
// or xor map, reproduces the s x s block at (rx, ry) exactly, with a domain
// that does not overlap it. It is the definition the matcher implements.
func bruteExists(orig *matrix.Dense, s, scale, stride, rx, ry int) bool {
	pw, ph := orig.Width(), orig.Height()
	side := scale * s
	tgt := make([]byte, s*s)
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			tgt[y*s+x] = orig.At(rx+x, ry+y)
		}
	}
	for dy := 0; dy+side <= ph; dy += stride {
		for dx := 0; dx+side <= pw; dx += stride {
			if overlaps(dx, dy, side, rx, ry, s) {
				continue
			}
			if scale == 1 && !copyIsCausal(dx, dy, s, rx, ry) {
				continue // same-scale copies read only earlier data
			}
			dec := matrix.NewDense(s, s)
			for y := 0; y < s; y++ {
				for x := 0; x < s; x++ {
					dec.Set(x, y, orig.At(dx+scale*x, dy+scale*y))
				}
			}
			for iso := transform.Isometry(0); iso < transform.NumIsometries; iso++ {
				o := matrix.NewDense(s, s)
				transform.Apply(iso, o, dec)
				for _, kind := range []transform.MapKind{transform.MapAdd, transform.MapXor} {
					var c byte
					if kind == transform.MapAdd {
						c = tgt[0] - o.Pix()[0]
					} else {
						c = tgt[0] ^ o.Pix()[0]
					}
					vm := transform.ValueMap{Kind: kind, C: c}
					ok := true
					for i, v := range o.Pix() {
						if vm.Apply(v) != tgt[i] {
							ok = false
							break
						}
					}
					if ok {
						return true
					}
				}
			}
		}
	}
	return false
}

func newTestEncoder(m *matrix.Dense, opt Options) *encoder {
	src := matrix.Bytes(m)
	c := &Compressed{Width: m.Width(), Height: m.Height(), Block: opt.Block, Iterations: opt.Iterations, Start: mostFrequent(src)}
	pw, ph := c.paddedDims()
	orig := matrix.NewDense(pw, ph)
	orig.Fill(c.Start)
	for y := 0; y < m.Height(); y++ {
		copy(orig.Row(y), src[y*m.Width():(y+1)*m.Width()])
	}
	st := Stats{Cells: len(src)}
	return &encoder{opt: opt, c: c, orig: orig, st: &st, w: m.Width(), h: m.Height(), pw: pw, ph: ph, workers: 4}
}

// For every node, the matcher finds a recipe exactly when brute force says one
// exists (in either kind of domain, or only the decimated kind when same-scale
// copies are off), and any recipe it returns really reproduces the node.
func TestMatcherAgreesWithBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	inputs := map[string]*matrix.Dense{}
	for _, n := range []int{32, 48} {
		d := matrix.NewDense(n, n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				if x&y == 0 {
					d.Set(x, y, 255)
				}
			}
		}
		inputs[("sierpinski")+string(rune('0'+n/16))] = d
	}
	low := matrix.NewDense(40, 40) // few distinct values: many accidental matches, flat regions
	for i := range low.Pix() {
		low.Pix()[i] = byte(rng.Intn(3)) * 40
	}
	inputs["low-entropy"] = low
	rows := matrix.NewDense(48, 32) // rows repeat with a value shift: add-map relations
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			rows.Set(x, y, byte((x*5)%11)+byte(y/8*3))
		}
	}
	inputs["shifted-rows"] = rows
	tiles := matrix.NewDense(48, 48) // a tile map: 8x8 tiles drawn from a small palette
	palette := make([][]byte, 4)
	for i := range palette {
		palette[i] = make([]byte, 64)
		rng.Read(palette[i])
	}
	for ty := 0; ty < 6; ty++ {
		for tx := 0; tx < 6; tx++ {
			p := palette[rng.Intn(4)]
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					tiles.Set(tx*8+x, ty*8+y, p[y*8+x])
				}
			}
		}
	}
	inputs["tilemap"] = tiles

	for _, noCopy := range []bool{false, true} {
		opt := DefaultOptions()
		opt.Block, opt.MinBlock, opt.Stride = 8, 4, 4
		opt.NoSameScale = noCopy
		for name, m := range inputs {
			e := newTestEncoder(m, opt)
			for _, s := range opt.sizes() {
				stride := max(1, opt.Stride*s/opt.Block)
				copyStride := stride
				if noCopy {
					copyStride = 0
				}
				lv := newLevelData(s, e.pw, e.ph, stride, copyStride)
				e.matchLevel(lv)
				for ni := 0; ni < lv.nx*lv.ny; ni++ {
					rx, ry := (ni%lv.nx)*s, (ni/lv.nx)*s
					if lv.skip[ni] == 0 {
						if lv.recipe[ni] != noRecipe {
							t.Errorf("%s s=%d node (%d,%d): recipe for a node that needs none", name, s, rx, ry)
						}
						continue
					}
					want := bruteExists(e.orig, s, 2, stride, rx, ry)
					if !noCopy {
						want = want || bruteExists(e.orig, s, 1, stride, rx, ry)
					}
					tr, got := e.recipeFor(lv, ni, rx, ry)
					if got != want {
						t.Errorf("%s noCopy=%v s=%d node (%d,%d): matcher found=%v, brute force says %v", name, noCopy, s, rx, ry, got, want)
						continue
					}
					if got { // the recipe must reproduce the block exactly
						if noCopy && tr.SameScale {
							t.Fatalf("%s s=%d node (%d,%d): same-scale recipe although they are off", name, s, rx, ry)
						}
						next := matrix.NewDense(e.pw, e.ph)
						scale := 2
						if tr.SameScale {
							scale = 1
						}
						applyFused(next, e.orig, &tr, newBlockGeom(s, scale))
						for y := 0; y < s; y++ {
							for x := 0; x < s; x++ {
								if next.At(rx+x, ry+y) != e.orig.At(rx+x, ry+y) {
									t.Fatalf("%s s=%d node (%d,%d): recipe does not reproduce the block", name, s, rx, ry)
								}
							}
						}
					}
				}
			}
		}
	}
}

// The whole encoder, one tile, any number of goroutines: same bytes.
func TestEncodeIsIndependentOfWorkerCount(t *testing.T) {
	for name, m := range map[string]matrix.Matrix{
		"sierpinski-256": sierpinski(256),
		"repeated-rows":  repeatedRows(180, 130),
		"smooth":         smoothField(200, 150),
	} {
		var ref []byte
		for _, w := range []int{1, 2, 3, 8, 64} {
			opt := DefaultOptions()
			opt.Workers, opt.Tile = w, 1024 // one tile, so only inner parallelism varies
			tl, _, _, err := EncodeTiled(m, opt)
			if err != nil {
				t.Fatal(err)
			}
			blob, _ := tl.MarshalBinary()
			if ref == nil {
				ref = blob
			} else if !bytes.Equal(ref, blob) {
				t.Fatalf("%s: %d workers gave different bytes than 1", name, w)
			}
		}
	}
}

func TestQueryTableGroupsEqualKeys(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, n := range []int{1, 5, 100, 5000, 70000} {
		keys := make([]uint64, n)
		for i := range keys {
			keys[i] = nz(uint64(rng.Intn(max(1, n/3))) * 0x9E3779B97F4A7C15)
		}
		for _, w := range []int{1, 4} {
			q, group := buildQueryTable(keys, w)
			for i, k := range keys {
				sh, local, ok := q.find(k)
				if !ok {
					t.Fatalf("n=%d: key %d not found", n, i)
				}
				if group[i] != uint32(sh)<<24|local {
					t.Fatalf("n=%d: item %d group %#x, find says %#x", n, i, group[i], uint32(sh)<<24|local)
				}
			}
			byKey := map[uint64]uint32{}
			for i, k := range keys {
				if g, seen := byKey[k]; seen && g != group[i] {
					t.Fatalf("n=%d: equal keys got different groups", n)
				}
				byKey[k] = group[i]
			}
			distinct := map[uint32]uint64{}
			for k, g := range byKey {
				if other, dup := distinct[g]; dup && other != k {
					t.Fatalf("n=%d: different keys share group %#x", n, g)
				}
				distinct[g] = k
			}
		}
	}
}

func TestCandidateSlotKeepsLowestAndHighest(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for trial := 0; trial < 200; trial++ {
		n := 1 + rng.Intn(60)
		ids := rng.Perm(1000)[:n]
		var c candSlot
		c.lowMax.Store(^uint32(0))
		for _, id := range ids {
			c.offer(uint32(id))
		}
		sorted := append([]int(nil), ids...)
		for i := 1; i < len(sorted); i++ {
			for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			}
		}
		want := map[uint32]bool{}
		for i := 0; i < candK && i < n; i++ {
			want[uint32(sorted[i])] = true
			want[uint32(sorted[n-1-i])] = true
		}
		got := c.candidates(nil)
		if len(got) != len(want) {
			t.Fatalf("n=%d: kept %d ids, want %d (%v vs %v)", n, len(got), len(want), got, want)
		}
		for i, v := range got {
			if !want[v] || (i > 0 && got[i-1] >= v) {
				t.Fatalf("n=%d: unexpected or unsorted candidates %v", n, got)
			}
		}
	}
}
