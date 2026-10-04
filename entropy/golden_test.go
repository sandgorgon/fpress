package entropy

// Golden values for TestEncodingIsDeterministic: the length and FNV-1a hash
// of the encoding of a fixed input. If these change, previously written
// streams stop decoding.
const (
	goldenLen        = 902
	goldenSum uint32 = 0xaec509e6
)

// Golden values for TestEncodingIsDeterministicLarge.
const (
	goldenLargeLen        = 10486
	goldenLargeSum uint32 = 0xc45bd94b
)
