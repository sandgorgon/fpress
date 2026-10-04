package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fpress/codec"
)

func gradientFile(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	data := make([]byte, 100*60)
	for y := 0; y < 60; y++ {
		for x := 0; x < 100; x++ {
			data[y*100+x] = byte(x*3 + y)
		}
	}
	path := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestCompressDecompressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in, data := gradientFile(t, dir)
	packed, out := filepath.Join(dir, "x.fpr"), filepath.Join(dir, "out.bin")

	if err := runCompress([]string{in, packed}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(packed)
	if err != nil || st.Size() >= int64(len(data))/10 {
		t.Fatalf("compressed file missing or not small: %v %v", st, err)
	}
	if err := runDecompress([]string{packed, out}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, data) {
		t.Fatal("decompressed file differs from the original")
	}
}

func TestCompressFlags(t *testing.T) {
	dir := t.TempDir()
	in, data := gradientFile(t, dir)
	for _, flags := range [][]string{
		{"-no-prep"}, {"-no-fractal"}, {"-width", "100"}, {"-segment", "1024"}, {"-segment", "-1"},
		{"-no-reject"}, {"-tile", "32", "-block", "8", "-min-block", "4"}, {"-workers", "2", "-v"},
	} {
		packed, out := filepath.Join(dir, "p.fpr"), filepath.Join(dir, "o.bin")
		if err := runCompress(append(append([]string(nil), flags...), in, packed)); err != nil {
			t.Fatalf("%v: %v", flags, err)
		}
		if err := runDecompress([]string{packed, out}); err != nil {
			t.Fatalf("%v: %v", flags, err)
		}
		if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
			t.Fatalf("%v: round trip mismatch", flags)
		}
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	in, _ := gradientFile(t, dir)
	if err := runCompress([]string{in}); err == nil {
		t.Error("compress with one argument should fail")
	}
	if err := runDecompress([]string{in}); err == nil {
		t.Error("decompress with one argument should fail")
	}
	if err := runCompress([]string{filepath.Join(dir, "missing"), filepath.Join(dir, "o")}); err == nil {
		t.Error("missing input should fail")
	}
	// A file that is not a container, and a damaged container.
	if err := runDecompress([]string{in, filepath.Join(dir, "o")}); err == nil {
		t.Error("decompressing a non-container should fail")
	}
	packed := filepath.Join(dir, "p.fpr")
	if err := runCompress([]string{in, packed}); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(packed)
	// Damage the stored checksum (after magic, version, mode and the length
	// varint): that always fails, whereas a flipped last payload byte of an
	// arithmetic-coded stream can be harmless slack.
	var lenBuf [binary.MaxVarintLen64]byte
	crcAt := 6 + binary.PutUvarint(lenBuf[:], 6000)
	blob[crcAt] ^= 0xff
	os.WriteFile(packed, blob, 0o644)
	out := filepath.Join(dir, "o")
	if err := runDecompress([]string{packed, out}); err == nil {
		t.Error("damaged container should fail")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("no output file should be written when decompression fails")
	}
}

func TestBench(t *testing.T) {
	dir := t.TempDir()
	in, _ := gradientFile(t, dir)
	if err := runBench([]string{in}); err != nil {
		t.Fatal(err)
	}
	if err := runBench([]string{"-head", "2000", "-ext", in}); err != nil {
		t.Fatal(err)
	}
	if err := runBench(nil); err == nil {
		t.Error("bench with no files should fail")
	}
	if err := runBench([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("bench of a missing file should fail")
	}
}

func parseFlags(t *testing.T, args ...string) codecOptions {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	get := codecFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	o, err := get()
	return codecOptions{o, err}
}

type codecOptions struct {
	codec.Options
	err error
}

// A flag's default value must never undo a preset; only a flag the user gave overrides it.
func TestPresetFlagsAndOverrides(t *testing.T) {
	fast := codec.PresetOptions(codec.PresetFast)
	o := parseFlags(t, "-preset", "fast")
	if o.err != nil || o.SegmentSize != fast.SegmentSize || !o.SkipWhole || !o.DisableFractal || o.CMAttempts != 3 || o.SearchSample != fast.SearchSample {
		t.Errorf("-preset fast alone did not give the preset: %+v (%v)", o.Options, o.err)
	}
	o = parseFlags(t, "-preset", "fast", "-segment", "2048", "-cm-attempts", "2")
	if o.SegmentSize != 2048 || o.CMAttempts != 2 || !o.SkipWhole {
		t.Errorf("explicit flags should override the preset's values only: %+v", o.Options)
	}
	o = parseFlags(t, "-preset", "fastest")
	if !o.DisableCM || o.SegmentSize != 1<<20 {
		t.Errorf("-preset fastest: %+v", o.Options)
	}
	o = parseFlags(t, "-preset", "fastest", "-no-cm=false")
	if o.DisableCM {
		t.Error("-no-cm=false should re-enable the coder under -preset fastest")
	}
	if o = parseFlags(t); o.Options != codec.DefaultOptions() {
		t.Errorf("no flags should give the default options: %+v", o.Options)
	}
	if o = parseFlags(t, "-preset", "warp"); o.err == nil {
		t.Error("unknown preset accepted")
	}
}

func TestCompressWithEachPreset(t *testing.T) {
	dir := t.TempDir()
	in, data := gradientFile(t, dir)
	for _, p := range []string{"default", "fast", "fastest"} {
		packed, out := filepath.Join(dir, p+".fpr"), filepath.Join(dir, p+".out")
		if err := runCompress([]string{"-preset", p, in, packed}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if err := runDecompress([]string{packed, out}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
			t.Fatalf("%s: round trip mismatch", p)
		}
	}
	if err := runCompress([]string{"-preset", "warp", in, filepath.Join(dir, "x")}); err == nil {
		t.Error("unknown preset should fail")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"4096": 4096, "1K": 1 << 10, "512M": 512 << 20, "8G": 8 << 30, "1.5G": 3 << 29, " 2g ": 2 << 30, "1T": 1 << 40} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5", "12X", "1e30G", "G"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
	if got := formatSize(64 << 20); got != "64M" {
		t.Errorf("formatSize = %q", got)
	}
}

func TestMemoryAndBigSegmentFlags(t *testing.T) {
	o := parseFlags(t, "-memory", "1G", "-big-segment", "64K")
	if o.err != nil || o.Fractal.MemoryLimit != 1<<30 || o.BigSegment != 64<<10 {
		t.Errorf("flags not applied: %+v (%v)", o.Options, o.err)
	}
	if o = parseFlags(t, "-memory", "lots"); o.err == nil {
		t.Error("a bad -memory value was accepted")
	}
	if o = parseFlags(t, "-big-segment", "x"); o.err == nil {
		t.Error("a bad -big-segment value was accepted")
	}
}

// A file larger than the super-segment size takes the streaming path, and
// round trips; the output is built under a temporary name and renamed.
func TestCompressLargeFileInSuperSegments(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 0, 300000)
	for len(data) < 300000 {
		data = append(data, bytes.Repeat([]byte("super-segments make any size possible. "), 50)...)
		data = append(data, byte(len(data)), byte(len(data)>>8))
	}
	in := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	packed, out := filepath.Join(dir, "big.fpr"), filepath.Join(dir, "big.out")
	if err := runCompress([]string{"-preset", "fast", "-big-segment", "64K", "-v", in, packed}); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(packed)
	if len(blob) < 6 || blob[5] != 6 { // ModeBig
		t.Fatalf("expected a big container, mode byte %d", blob[5])
	}
	if err := runDecompress([]string{packed, out}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fpress-") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
	// Corrupt it: decompression fails, no output file appears, nothing is left over.
	blob[len(blob)/2] ^= 0xff
	os.WriteFile(packed, blob, 0o644)
	os.Remove(out)
	if err := runDecompress([]string{packed, out}); err == nil {
		// harmless slack is possible in principle; then the output must be right
		if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
			t.Fatal("damaged container produced wrong output without an error")
		}
	} else if _, err := os.Stat(out); err == nil {
		t.Error("an output file was left after a failed decompression")
	}
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fpress-") {
			t.Errorf("temporary file %s left behind after a failure", e.Name())
		}
	}
}

func TestOpenInputSpillsStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	payload := bytes.Repeat([]byte("from a pipe "), 5000)
	go func() { w.Write(payload); w.Close() }()
	in, size, cleanup, err := openInput("-")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if size != int64(len(payload)) {
		t.Fatalf("size %d, want %d", size, len(payload))
	}
	got := make([]byte, size)
	if n, err := in.ReadAt(got, 0); n != len(got) && err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("spilled content differs")
	}
}

func TestDecompressMemoryFlag(t *testing.T) {
	dir := t.TempDir()
	in, data := gradientFile(t, dir)
	packed, out := filepath.Join(dir, "g.fpr"), filepath.Join(dir, "g.out")
	if err := runCompress([]string{in, packed}); err != nil {
		t.Fatal(err)
	}
	if err := runDecompress([]string{"-memory", "512M", packed, out}); err != nil {
		t.Fatalf("a generous limit failed: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("wrong output under -memory")
	}
	if err := runDecompress([]string{"-memory", "lots", packed, out}); err == nil {
		t.Error("a bad -memory value was accepted")
	}
	// A limit smaller than one piece: a clear error, and no output file.
	os.Remove(out)
	err := runDecompress([]string{"-memory", "1K", packed, out})
	if err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("got %v, want a memory error", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("an output file was left behind after the refusal")
	}
}

// The tool is one self-contained binary: only the standard library and this
// module's own packages, and no cgo (so no C library to link against).
func TestBuildsWithStandardLibraryOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available")
	}
	cmd := exec.Command(goTool, "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list failed: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if f[1] != "true" && !strings.HasPrefix(f[0], "fpress") {
			t.Errorf("dependency outside the standard library: %s", f[0])
		}
		if f[0] == "C" || f[0] == "runtime/cgo" {
			t.Errorf("binary depends on cgo: %s", f[0])
		}
	}
}
