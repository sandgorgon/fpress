package codec_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sandgorgon/fpress/codec"
)

// Compress and Decompress work on byte slices held in memory.
func Example() {
	data := bytes.Repeat([]byte("the same line, again and again\n"), 1000)

	packed, err := codec.Compress(data, codec.PresetOptions(codec.PresetFast))
	if err != nil {
		panic(err)
	}
	back, err := codec.Decompress(packed)
	if err != nil {
		panic(err)
	}
	fmt.Println("100 times smaller:", len(packed)*100 < len(data))
	fmt.Println("came back intact:", bytes.Equal(back, data))
	// Output:
	// 100 times smaller: true
	// came back intact: true
}

// CompressStream and DecompressStreamOpt work on files of any size; memory
// follows the budget, not the file. This is what the fpress command does.
func ExampleCompressStream() {
	dir, _ := os.MkdirTemp("", "fpress-example")
	defer os.RemoveAll(dir)
	name := filepath.Join(dir, "data.bin")
	os.WriteFile(name, bytes.Repeat([]byte{1, 2, 3, 4}, 50000), 0o644)

	// Compress data.bin into data.bin.fpr.
	in, err := os.Open(name)
	if err != nil {
		panic(err)
	}
	defer in.Close()
	st, _ := in.Stat()
	out, err := os.Create(name + ".fpr")
	if err != nil {
		panic(err)
	}
	opt := codec.PresetOptions(codec.PresetDefault)
	opt.Fractal.MemoryLimit = 2 << 30 // plan around 2 GiB
	if _, err := codec.CompressStream(out, in, st.Size(), opt); err != nil {
		panic(err)
	}
	if err := out.Close(); err != nil {
		panic(err)
	}

	// Decompress it again, refusing files that would need more than 1 GiB.
	packed, _ := os.Open(name + ".fpr")
	defer packed.Close()
	pst, _ := packed.Stat()
	var back bytes.Buffer
	n, err := codec.DecompressStreamOpt(&back, packed, pst.Size(), codec.DecodeOptions{MemoryLimit: 1 << 30})
	switch {
	case errors.Is(err, codec.ErrMemory):
		fmt.Println("needs more memory than allowed:", err)
	case err != nil: // codec.ErrCorrupt or codec.ErrChecksum: damaged input
		fmt.Println("damaged:", err)
	default:
		fmt.Println(n, "bytes restored")
	}
	// Output: 200000 bytes restored
}

// A Progress, read from another goroutine, shows how far a long call has got.
func ExampleProgress() {
	data := bytes.Repeat([]byte("some text to compress "), 20000)
	opt := codec.PresetOptions(codec.PresetFast)
	opt.Progress = new(codec.Progress)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := codec.Compress(data, opt); err != nil {
			panic(err)
		}
	}()
	// A real program would print this every half second or so:
	//   s := opt.Progress.Status()
	//   fmt.Printf("\r%d of %d bytes", s.Done, s.Total)
	<-done
	s := opt.Progress.Status()
	fmt.Println(s.Done, "of", s.Total, "bytes")
	// Output: 440000 of 440000 bytes
}
