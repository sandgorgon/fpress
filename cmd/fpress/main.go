// Command fpress compresses files with fractal-style matrix compression.
//
//	fpress compress   [flags] in out
//	fpress decompress in out
//	fpress bench      [flags] file...
//
// Use "-" for stdin or stdout.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"fpress/codec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "compress":
		err = runCompress(os.Args[2:])
	case "decompress":
		err = runDecompress(os.Args[2:])
	case "bench":
		err = runBench(os.Args[2:])
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "fpress: unknown command %q\n", os.Args[1])
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  fpress compress   [flags] in out    compress a file ("-" = stdin/stdout)
  fpress decompress [-memory 2G] in out  restore a file
  fpress bench      [flags] file...   compare stored / flate / prep / fractal sizes

compress and bench flags: -preset (default|fast|fastest) -memory -big-segment -width -block -iter -stride -max-fractal -tile -min-block -workers -segment -no-cm -max-cm -cm-attempts -no-reject -no-fractal -no-copy -no-gate -no-prep -video WxH[:format]
bench also takes -head N to use only the first N bytes of each file, and
-ext to add columns for xz -9e, zstd -19 and bzip2 -9 when they are installed.
`)
	os.Exit(2)
}

// codecFlags registers the tuning flags shared by compress and bench. The
// returned function builds the Options: the preset first, then only the flags
// the user actually gave, so a flag's default never undoes a preset.
func codecFlags(fs *flag.FlagSet) func() (codec.Options, error) {
	def := codec.DefaultOptions()
	preset := fs.String("preset", "default", "speed/size trade-off: default, fast or fastest")
	width := fs.Int("width", 0, "fold width in bytes (0 = automatic)")
	block := fs.Int("block", def.Fractal.Block, "range block size")
	iter := fs.Int("iter", def.Fractal.Iterations, "decoder iterations")
	stride := fs.Int("stride", def.Fractal.Stride, "spacing between candidate domain blocks")
	maxf := fs.Int("max-fractal", def.MaxFractalSize, "skip the fractal attempt above this many bytes (0 = no limit)")
	tile := fs.Int("tile", def.Fractal.Tile, "fractal tile side")
	minb := fs.Int("min-block", def.Fractal.MinBlock, "smallest range block (quad-tree depth)")
	workers := fs.Int("workers", 0, "parallel tiles/segments (0 = all CPUs)")
	seg := fs.Int("segment", 0, "segment size for segmented mode (0 = preset's, -1 = off)")
	noCM := fs.Bool("no-cm", false, "disable the context-mixing coder")
	maxCM := fs.Int("max-cm", def.MaxCMSize, "skip the context-mixing coder above this many bytes")
	attempts := fs.Int("cm-attempts", 0, "layouts the context-mixing coder tries per block (0 = automatic)")
	memory := fs.String("memory", "", "memory budget the encoder plans tiles, parallelism and super-segments around (e.g. 4G, 512M; default 8G)")
	bigSeg := fs.String("big-segment", "", "super-segment size for very large inputs (e.g. 64M; default: from the memory budget)")
	noCopy := fs.Bool("no-copy", false, "disable same-scale (2D copy) fractal recipes")
	noGate := fs.Bool("no-gate", false, "always run the fractal step (do not skip it when the data shows no self-similarity)")
	noRej := fs.Bool("no-reject", false, "disable the quick reject for incompressible data")
	no := fs.Bool("no-fractal", false, "skip the fractal attempt")
	noPrep := fs.Bool("no-prep", false, "skip the prep (width/delta/bit-plane) search")
	video := fs.String("video", "", "input is raw video, headerless frames: WIDTHxHEIGHT[:FORMAT] ("+codec.VideoFormats+")")
	return func() (codec.Options, error) {
		p, err := codec.ParsePreset(*preset)
		if err != nil {
			return codec.Options{}, err
		}
		o := codec.PresetOptions(p)
		given := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
		set := func(name string, apply func()) {
			if given[name] {
				apply()
			}
		}
		set("width", func() { o.Width = *width })
		set("block", func() { o.Fractal.Block = *block })
		set("iter", func() { o.Fractal.Iterations = *iter })
		set("stride", func() { o.Fractal.Stride = *stride })
		set("max-fractal", func() { o.MaxFractalSize = *maxf })
		set("tile", func() { o.Fractal.Tile = *tile })
		set("min-block", func() { o.Fractal.MinBlock = *minb })
		set("workers", func() { o.Fractal.Workers = *workers })
		set("segment", func() { o.SegmentSize = *seg })
		set("no-cm", func() { o.DisableCM = *noCM })
		set("max-cm", func() { o.MaxCMSize = *maxCM })
		set("cm-attempts", func() { o.CMAttempts = *attempts })
		set("no-reject", func() { o.NoQuickReject = *noRej })
		set("no-fractal", func() { o.DisableFractal = *no })
		set("no-copy", func() { o.Fractal.NoSameScale = *noCopy })
		set("no-gate", func() { o.NoFractalGate = *noGate })
		set("no-prep", func() { o.DisablePrep = *noPrep })
		if given["video"] {
			w, rows, err := codec.ParseVideo(*video)
			if err != nil {
				return o, fmt.Errorf("-video: %w", err)
			}
			if given["width"] && o.Width != w {
				return o, errors.New("-video sets the width itself; do not combine it with -width")
			}
			o.Width, o.FrameRows = w, rows
			o.DisableFractal = true // frame differencing does the job; the fractal search only costs time here
		}
		if given["memory"] {
			n, err := parseSize(*memory)
			if err != nil {
				return o, fmt.Errorf("-memory: %w", err)
			}
			o.Fractal.MemoryLimit = n
		}
		if given["big-segment"] {
			n, err := parseSize(*bigSeg)
			if err != nil {
				return o, fmt.Errorf("-big-segment: %w", err)
			}
			o.BigSegment = int(min(n, 1<<30))
		}
		return o, nil
	}
}

func runCompress(args []string) error {
	fs := flag.NewFlagSet("compress", flag.ExitOnError)
	get := codecFlags(fs)
	verbose := fs.Bool("v", false, "print a summary to stderr")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("compress needs: in out")
	}
	opt, err := get()
	if err != nil {
		return err
	}
	in, size, cleanup, err := openInput(fs.Arg(0))
	if err != nil {
		return err
	}
	defer cleanup()
	start := time.Now()
	var rep codec.Report
	err = writeOutput(fs.Arg(1), func(w io.Writer) error {
		var err error
		rep, err = codec.CompressStream(w, in, size, opt)
		return err
	})
	if err != nil {
		return err
	}
	if *verbose {
		out := int64(rep.Out)
		if rep.OutBytes > 0 {
			out = rep.OutBytes
		}
		extra := ""
		if rep.BigSegments > 0 {
			extra = fmt.Sprintf(", %d super-segments of %s", rep.BigSegments, formatSize(int64(codec.BigSegmentSize(opt))))
		}
		fmt.Fprintf(os.Stderr, "%d -> %d bytes (%s%s) in %v\n", size, out, rep.Chosen, extra, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func runDecompress(args []string) error {
	fs := flag.NewFlagSet("decompress", flag.ExitOnError)
	memory := fs.String("memory", "", "memory budget to decode within (e.g. 2G, 512M): limits parallelism, and refuses files whose pieces cannot fit (default: no limit)")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("decompress needs: in out")
	}
	var dopt codec.DecodeOptions
	if *memory != "" {
		n, err := parseSize(*memory)
		if err != nil {
			return fmt.Errorf("-memory: %w", err)
		}
		dopt.MemoryLimit = n
	}
	in, size, cleanup, err := openInput(fs.Arg(0))
	if err != nil {
		return err
	}
	defer cleanup()
	return writeOutput(fs.Arg(1), func(w io.Writer) error {
		_, err := codec.DecompressStreamOpt(w, in, size, dopt)
		return err
	})
}

// openInput returns a random-access reader for path and its size. Standard
// input cannot be read twice or at an offset, so it is first copied to a
// temporary file (removed by the returned cleanup).
func openInput(path string) (io.ReaderAt, int64, func(), error) {
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, 0, nil, err
		}
		return f, st.Size(), func() { f.Close() }, nil
	}
	tmp, err := os.CreateTemp("", "fpress-stdin-*")
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	n, err := io.Copy(tmp, os.Stdin)
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return tmp, n, cleanup, nil
}

// writeOutput runs write against path. A file is written under a temporary
// name in the same directory and renamed into place only on success, so a
// failed run never leaves a partial output behind; "-" is standard output.
func writeOutput(path string, write func(io.Writer) error) error {
	if path == "-" {
		bw := bufio.NewWriterSize(os.Stdout, 1<<20)
		if err := write(bw); err != nil {
			return err
		}
		return bw.Flush()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fpress-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	bw := bufio.NewWriterSize(tmp, 1<<20)
	if err := write(bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	ok = true
	return os.Rename(tmp.Name(), path)
}

// parseSize reads a size like 4096, 512M, 8G or 1.5G (binary multiples).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for suffix, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(s, suffix) {
			mult, s = m, strings.TrimSuffix(s, suffix)
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 || v*float64(mult) > float64(1<<62) {
		return 0, fmt.Errorf("invalid size")
	}
	return int64(v * float64(mult)), nil
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dM", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dK", n>>10)
	}
	return fmt.Sprint(n)
}

// external compressors used as a reference by "bench -ext".
var externals = []struct {
	name string
	args []string
}{
	{"xz", []string{"-9e", "-c"}},
	{"zstd", []string{"-19", "-c", "-q"}},
	{"bzip2", []string{"-9", "-c"}},
}

// externalSize pipes data through an installed compressor and returns the
// output size, or -1 if the tool is missing or fails.
func externalSize(name string, args []string, data []byte) int {
	path, err := exec.LookPath(name)
	if err != nil {
		return -1
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	return len(out)
}

func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	get := codecFlags(fs)
	head := fs.Int("head", 0, "use only the first N bytes of each file (0 = all)")
	ext := fs.Bool("ext", false, "also run xz, zstd and bzip2 as references")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("bench needs at least one file")
	}
	opt, err := get()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	header := "file\tsize\tflate\tprep\tfractal\tcm\tsegmented\tchosen\tout\tratio"
	if *ext {
		header += "\txz\tzstd\tbzip2"
	}
	fmt.Fprintln(tw, header+"\tdetail\ttime")
	size := func(n int) string {
		if n < 0 {
			return "-"
		}
		return fmt.Sprint(n)
	}
	for _, name := range fs.Args() {
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if *head > 0 && len(data) > *head {
			data = data[:*head]
		}
		start := time.Now()
		_, rep, err := codec.CompressReport(data, opt)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		elapsed := time.Since(start).Round(time.Millisecond)

		detail := ""
		switch {
		case rep.Rejected:
			detail = "quick reject"
		case rep.Chosen == codec.ModeCM:
			detail = fmt.Sprintf("w=%d %v", rep.CMWidth, rep.CMChain)
		case rep.Chosen == codec.ModePrep:
			detail = fmt.Sprintf("w=%d %v", rep.PrepWidth, rep.PrepChain)
		case rep.Chosen == codec.ModeFractal:
			st, ts := rep.FractalStats, rep.FractalTiles
			detail = fmt.Sprintf("w=%d %v tiles(f/z/s)=%d/%d/%d exact=%.0f%% recipes=%d anchors=%d", rep.FractalWidth, rep.FractalChain,
				ts.Fractal, ts.Flate, ts.Stored, 100*float64(st.ExactCells)/float64(max(ts.FractalCells, 1)), st.Recipes, st.Anchors)
		case rep.Chosen == codec.ModeSegmented:
			m, total := rep.SegModes, 0
			for _, c := range m {
				total += c
			}
			detail = fmt.Sprintf("%d x %dK segments (stored/flate/fractal/prep/cm)=%d/%d/%d/%d/%d",
				total, rep.SegSize>>10, m[codec.ModeStored], m[codec.ModeFlate], m[codec.ModeFractal], m[codec.ModePrep], m[codec.ModeCM])
		}
		if rep.FractalGated {
			note := fmt.Sprintf("fractal skipped (probe: %.1f%% of blocks self-similar)", 100*rep.FractalProbe.Rate())
			if detail != "" {
				detail += " | "
			}
			detail += note
		}
		row := fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%.3f",
			filepath.Base(name), rep.OrigLen, size(rep.Flate), size(rep.PrepFlate), size(rep.Fractal), size(rep.CM), size(rep.Segmented),
			rep.Chosen, rep.Out, float64(rep.Out)/float64(max(rep.OrigLen, 1)))
		if *ext {
			for _, e := range externals {
				row += "\t" + size(externalSize(e.name, e.args, data))
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%v\n", row, detail, elapsed)
	}
	return tw.Flush()
}
