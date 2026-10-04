package fractal

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"reflect"
	"testing"

	"fpress/transform"
)

// randomTransforms makes a valid list for a pw x ph grid: a mix of raw
// anchors and recipes of several sizes, in no particular order, so position
// deltas go both ways.
func randomTransforms(rng *rand.Rand, n, pw, ph int) []Transform {
	ts := make([]Transform, 0, n)
	for len(ts) < n {
		s := []int{1, 2, 4, 8}[rng.Intn(4)]
		if 2*s > pw || 2*s > ph {
			continue
		}
		t := Transform{Size: s, RX: rng.Intn(pw - s + 1), RY: rng.Intn(ph - s + 1)}
		if rng.Intn(4) == 0 {
			t.Raw = true
			t.Data = make([]byte, s*s)
			rng.Read(t.Data)
		} else {
			t.DX, t.DY = rng.Intn(pw-2*s+1), rng.Intn(ph-2*s+1)
			t.Iso = transform.Isometry(rng.Intn(8))
			t.SameScale = rng.Intn(3) == 0
			switch rng.Intn(3) {
			case 1:
				t.Map = transform.ValueMap{Kind: transform.MapAdd, C: byte(1 + rng.Intn(255))}
			case 2:
				t.Map = transform.ValueMap{Kind: transform.MapXor, C: byte(1 + rng.Intn(255))}
			}
		}
		ts = append(ts, t)
	}
	return ts
}

func TestRecordsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, n := range []int{0, 1, 2, 7, 50, 400} {
		ts := randomTransforms(rng, n, 200, 150)
		blob := encodeRecords(ts)
		got, err := decodeRecords(blob, len(ts), 200, 150)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(ts) == 0 {
			if len(got) != 0 {
				t.Fatalf("n=0: got %d records", len(got))
			}
			continue
		}
		if !reflect.DeepEqual(got, ts) {
			t.Fatalf("n=%d: records changed in the round trip", n)
		}
	}
}

func TestPackBlobModes(t *testing.T) {
	roundTrip := func(name string, tb []byte, wantMode int) {
		t.Helper()
		p := packBlob(tb, 0)
		if wantMode >= 0 && int(p[0]) != wantMode {
			t.Errorf("%s: mode %d, want %d", name, p[0], wantMode)
		}
		got, err := unpackBlob(p, int64(len(tb)), 0)
		if err != nil || !bytes.Equal(got, tb) {
			t.Fatalf("%s: round trip failed: %v", name, err)
		}
		if len(p) > len(tb)+6 {
			t.Errorf("%s: packed %d bytes from %d", name, len(p), len(tb))
		}
	}
	roundTrip("empty", nil, blobStored)
	noise := make([]byte, 3000)
	rand.New(rand.NewSource(2)).Read(noise)
	roundTrip("noise", noise, blobStored)
	structured := encodeRecords(randomTransforms(rand.New(rand.NewSource(3)), 300, 400, 300))
	roundTrip("records", structured, -1)
	repetitive := bytes.Repeat([]byte{0, 0, 0, 1, 9, 9, 9, 9}, 400)
	p := packBlob(repetitive, 0)
	if p[0] == blobStored || len(p) > len(repetitive)/20 {
		t.Errorf("repetitive blob: mode %d, %d bytes from %d", p[0], len(p), len(repetitive))
	}
	roundTrip("repetitive", repetitive, -1)
}

func TestUnpackBlobRejectsMalformed(t *testing.T) {
	tb := encodeRecords(randomTransforms(rand.New(rand.NewSource(4)), 100, 300, 300))
	valid := map[string][]byte{
		"stored": append(binary.AppendUvarint([]byte{blobStored}, uint64(len(tb))), tb...),
		"flate":  append(binary.AppendUvarint([]byte{blobFlate}, uint64(len(tb))), deflate(tb)...),
	}
	cm := packBlob(bytes.Repeat([]byte("record data "), 200), 0)
	if cm[0] == blobCM {
		valid["cm"] = cm
	}
	limit := int64(len(tb)) + 64
	for name, p := range valid {
		want := tb
		if name == "cm" {
			want, limit = bytes.Repeat([]byte("record data "), 200), 1<<20
		}
		got, err := unpackBlob(p, limit, 0)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: valid blob rejected: %v", name, err)
		}
		for n := 0; n < len(p); n++ {
			if _, err := unpackBlob(p[:n], limit, 0); err == nil {
				t.Errorf("%s: truncation to %d/%d accepted", name, n, len(p))
				break
			}
		}
		if _, err := unpackBlob(append(append([]byte(nil), p...), 0), limit, 0); err == nil {
			t.Errorf("%s: trailing byte accepted", name)
		}
	}
	huge := binary.AppendUvarint([]byte{blobFlate}, 1<<28)
	for name, p := range map[string][]byte{
		"empty":             nil,
		"unknown mode":      {9, 0},
		"bad length":        {blobStored, 0xff},
		"over the limit":    append(binary.AppendUvarint([]byte{blobStored}, 1<<40), 0),
		"stored wrong len":  append(binary.AppendUvarint([]byte{blobStored}, 10), 1, 2, 3),
		"implausible ratio": append(huge, deflate([]byte{1})...),
	} {
		if _, err := unpackBlob(p, 1<<30, 0); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDecodeRecordsRejectsMalformed(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	ts := randomTransforms(rng, 60, 100, 100)
	blob := encodeRecords(ts)
	if _, err := decodeRecords(blob, len(ts), 100, 100); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(blob); n++ {
		if _, err := decodeRecords(blob[:n], len(ts), 100, 100); err == nil {
			t.Fatalf("truncation to %d/%d accepted", n, len(blob))
		}
	}
	if _, err := decodeRecords(append(append([]byte(nil), blob...), 0), len(ts), 100, 100); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := decodeRecords(blob, len(ts)+1, 100, 100); err == nil {
		t.Error("wrong record count accepted")
	}
	// Reserved flag bits.
	// (0x40 is no longer reserved: it marks a same-scale recipe. A raw block, flag
	// bit 0, may carry no other bit, and bit 7 is never valid.)
	for _, f := range []byte{0x80, 0xC0, 0x41, 0x03, 0xff} {
		bad := append([]byte(nil), blob...)
		bad[0] = f
		if _, err := decodeRecords(bad, len(ts), 100, 100); err == nil {
			t.Errorf("flags %#x accepted", f)
		}
	}
	// A raw anchor that claims to be larger than the grid must fail before allocating.
	huge := []Transform{{Raw: true, Size: 1 << 20, Data: nil}}
	if _, err := decodeRecords(encodeRecords(huge), 1, 100, 100); err == nil {
		t.Error("oversized raw block accepted")
	}
	// Positions are deltas; one that goes below zero is invalid.
	neg := []Transform{{Size: 1, RX: 0, RY: 0}}
	b := encodeRecords(neg)
	b[2] = byte(zigzag(-5)) // the rx delta (flags byte, size byte, then rx)
	if _, err := decodeRecords(b, 1, 100, 100); err == nil {
		t.Error("negative position accepted")
	}
}

func FuzzDecodeRecords(f *testing.F) {
	f.Add(encodeRecords(randomTransforms(rand.New(rand.NewSource(6)), 20, 64, 64)), uint8(20))
	f.Add([]byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, blob []byte, count uint8) {
		decodeRecords(blob, int(count), 64, 64) // an error is fine; a panic is not
	})
}

// legacyRecords is the previous layout (one record after another, absolute
// positions), kept here only to show what the new one saves.
func legacyRecords(ts []Transform) []byte {
	var tb []byte
	for i := range ts {
		t := &ts[i]
		if t.Raw {
			tb = append(tb, 1)
		} else {
			tb = append(tb, byte(t.Iso)<<1|byte(t.Map.Kind)<<4)
		}
		tb = binary.AppendUvarint(tb, uint64(t.RX))
		tb = binary.AppendUvarint(tb, uint64(t.RY))
		tb = binary.AppendUvarint(tb, uint64(t.Size))
		if t.Raw {
			tb = append(tb, t.Data...)
			continue
		}
		tb = binary.AppendUvarint(tb, uint64(t.DX))
		tb = binary.AppendUvarint(tb, uint64(t.DY))
		if t.Map.Kind != transform.MapIdentity {
			tb = append(tb, t.Map.C)
		}
	}
	return tb
}

func TestNewLayoutIsMuchSmallerThanTheOld(t *testing.T) {
	for _, n := range []int{256, 512, 1024} {
		opt := DefaultOptions()
		opt.Tile = 1024
		c, _, err := Encode(sierpinski(n), opt)
		if err != nil {
			t.Fatal(err)
		}
		old := len(deflate(legacyRecords(c.Transforms)))
		now := len(packBlob(encodeRecords(c.Transforms), 0))
		t.Logf("sierpinski %d: %d records, old layout+flate %d bytes, new %d bytes (%.0f%% smaller)",
			n, len(c.Transforms), old, now, 100*(1-float64(now)/float64(old)))
		if now*10 > old*7 {
			t.Errorf("sierpinski %d: new layout (%d) is not at least 30%% smaller than the old (%d)", n, now, old)
		}
	}
}
