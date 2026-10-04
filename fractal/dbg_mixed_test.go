package fractal

import (
	"os"
	"testing"

	"fpress/matrix"
)

func TestDbgMixed(t *testing.T) {
	b, err := os.ReadFile("/tmp/claude-1000/-home-sandgorgon-projects-fpress/a151217d-d858-4b04-8f7e-afdb37af9fa0/scratchpad/mixed.bin")
	if err != nil {
		t.Skip()
	}
	m := matrix.FromBytes(b, 256)
	for _, noCopy := range []bool{true, false} {
		opt := DefaultOptions()
		opt.NoSameScale, opt.Workers, opt.Tile, opt.NoSelfCheck = noCopy, 1, 4096, true
		c, st, err := Encode(m, opt)
		if err != nil {
			t.Fatal(err)
		}
		rec := encodeRecords(c.Transforms)
		bySize := map[int][2]int{} // size -> {copies, anchors}
		rawBytes := 0
		for _, tr := range c.Transforms {
			v := bySize[tr.Size]
			if tr.SameScale {
				v[0]++
			}
			if tr.Raw {
				v[1]++
				rawBytes += len(tr.Data)
			}
			bySize[tr.Size] = v
		}
		blob, _ := c.MarshalBinary()
		t.Logf("noCopy=%-5v blob %6d | transforms %5d: copies %4d anchors %4d (raw %6d B) recipes %4d demoted %3d rounds %2d | records raw %6d packed %6d | residual %5d (%d nonzero)",
			noCopy, len(blob), len(c.Transforms), st.CopyRecipes, st.Anchors, rawBytes, st.Recipes, st.Demoted, st.RefineRounds, len(rec), len(packBlob(rec, 1)), len(c.Residual), st.ResidualNonZero)
		if !noCopy {
			t.Logf("   by size {copies, anchors}: %v", bySize)
		}
	}
}
