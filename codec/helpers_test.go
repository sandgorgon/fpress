package codec

import (
	"compress/flate"
	"hash/crc32"
	"testing"

	"fpress/entropy"
	"fpress/fractal"
	"fpress/prep"
)

func crc(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// fractalContainer builds a fractal-mode container directly, regardless of
// whether Compress would have chosen that mode for this data.
func fractalContainer(t testing.TB, data []byte, width int) []byte {
	return fractalContainerChain(t, data, width, nil)
}

func fractalContainerChain(t testing.TB, data []byte, width int, chain prep.Chain) []byte {
	t.Helper()
	payload, _, _, _, _, err := fractalPayload(data, prep.Candidate{Width: width, Chain: chain}, fractal.DefaultOptions(), false)
	if err != nil {
		t.Fatalf("fractalPayload(width %d, chain %v): %v", width, chain, err)
	}
	return container(ModeFractal, len(data), crc(data), payload)
}

// prepContainer builds a prep+flate container directly.
func prepContainer(t testing.TB, data []byte, width int, chain prep.Chain) []byte {
	t.Helper()
	payload, err := prepFlatePayload(data, width, chain, flate.BestCompression)
	if err != nil {
		t.Fatalf("prepFlatePayload(width %d, chain %v): %v", width, chain, err)
	}
	return container(ModePrep, len(data), crc(data), payload)
}

// segmentedContainer builds a segmented container directly.
func segmentedContainer(t testing.TB, data []byte, size int) []byte {
	t.Helper()
	payload, _, err := encodeSegmented(data, size, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return container(ModeSegmented, len(data), crc(data), payload)
}

// cmContainer builds a context-mixing container directly (width 0 = plain stream).
func cmContainer(t testing.TB, data []byte, width int, chain prep.Chain) []byte {
	t.Helper()
	payload, err := cmPayload(data, width, chain)
	if err != nil {
		t.Fatalf("cmPayload(width %d, chain %v): %v", width, chain, err)
	}
	return container(ModeCM, len(data), crc(data), payload)
}

func entropyOf(data []byte, stride int) []byte { return entropy.Encode(data, stride) }
