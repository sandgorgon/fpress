package fractal

// Fingerprints of the encoder's output (see goldenCases).
//
// History: recorded from the single-threaded encoder before the parallel work;
// unchanged through the parallel rebuild and the parallel matcher (which are
// required to give identical bytes). Updated once, deliberately, when the
// context-mixing streams for the residual and the records became chunked
// (EncodeChunked): each such stream gained a one-byte marker, so every case that
// uses one grew by 1 byte per stream and the cases that do not
// (repeated-rows, gradient, exhaustive-mixed) did not change.
//
// The first three cases were added with same-scale (2D copy) recipes and run
// with them on; the other nine run with NoSameScale and are the proof that
// adding the feature changed nothing when it is off.
var goldenValues = map[string]struct {
	n   int
	sum uint64
}{
	"tilemap-copies":             {1401, 0x57a82a3e26d7a505},
	"gradient-copies":            {538, 0x74e0e224dadbbd4d},
	"mixed-copies":               {8222, 0x5c5a19a3607de5d7},
	"sierpinski-128":             {103, 0xc8338507840775e4},
	"sierpinski-256-tiles-of-64": {762, 0xf942f0292765a73f},
	"sierpinski-300-odd":         {214, 0x1635291f454758ed},
	"repeated-rows":              {122, 0x535a0156d78eb277},
	"sparse-noisy":               {516, 0xf0e119857425d7b3},
	"gradient":                   {538, 0x74e0e224dadbbd4d},
	"mixed-regions":              {8475, 0xf5c7f47b6796b9b4},
	"sierpinski-block8":          {122, 0x155eb8b7a7631151},
	"exhaustive-mixed":           {8563, 0xe399ac5c152cbe28},
}
