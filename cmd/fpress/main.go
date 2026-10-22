// Command fpress compresses files with fractal-style matrix compression.
//
//	fpress compress   [flags] in out
//	fpress decompress [flags] in out
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/sandgorgon/fpress/codec"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "compress":
		err = runCompress(os.Args[2:])
	case "decompress":
		err = runDecompress(os.Args[2:])
	case "bench":
		err = runBench(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(versionString())
	case "-h", "-help", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "fpress: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	var ue usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.As(err, &ue):
		fmt.Fprintf(os.Stderr, "error: %v\nrun \"fpress %s -h\" for help\n", err, os.Args[1])
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// usageError is a mistake on the command line (exit status 2), as opposed to
// a failure while working (exit status 1).
type usageError string

func (e usageError) Error() string { return string(e) }

// version is the release number of this tool.
const version = "0.1.1"

func versionString() string {
	return fmt.Sprintf("fpress %s (container format %d, %s %s/%s)", version, codec.FormatVersion, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `fpress %s: lossless compression for any file

usage:
  fpress compress   [flags] IN OUT    compress IN into OUT
  fpress decompress [flags] IN OUT    restore the original file into OUT
  fpress bench      [flags] FILE...   try every method on each file and show the sizes
  fpress version                      print the version
  fpress help                         show this help

"-" as IN or OUT reads standard input or writes standard output.
Flags go before the file names.

common flags:
  -preset NAME     default (smallest, slowest), fast or fastest
  -memory SIZE     memory budget, such as 4G or 512M (compress default 8G;
                   on decompress: refuse files that cannot fit)
  -workers N       CPU cores to use (default: all)
  -video WxH[:FMT] the input is raw video frames, such as 1280x720:rgb24
  -v               compress: print the size, the method chosen and the time
  -q               compress: no progress line (shown only on a terminal)

examples:
  fpress compress data.bin data.bin.fpr
  fpress compress -preset fast server.log server.log.fpr
  fpress decompress data.bin.fpr data.bin
  tar cf - photos | fpress compress - photos.tar.fpr

"fpress compress -h" lists every flag, including the tuning flags.
Exit status: 0 done, 1 failed, 2 mistake on the command line.
`, version)
}

// Flags shown first in a command's help; every other flag is listed under
// "tuning flags", in this order.
var (
	commonFlags = []string{"preset", "memory", "workers", "video", "v", "q", "head", "ext"}
	tuningFlags = []string{
		"no-fractal", "no-cm", "no-prep", "no-copy", "no-gate", "no-reject",
		"width", "segment", "big-segment", "max-cm", "max-fractal", "cm-attempts",
		"block", "min-block", "stride", "iter", "tile",
	}
)

// setUsage gives fs a help text: the usage line and a description, then the
// command's flags, common ones first.
func setUsage(fs *flag.FlagSet, synopsis, about string) {
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "usage: fpress %s %s\n\n%s\n", fs.Name(), synopsis, about)
		group := func(title string, names []string) {
			printed := false
			for _, name := range names {
				f := fs.Lookup(name)
				if f == nil {
					continue
				}
				if !printed {
					fmt.Fprintf(w, "\n%s:\n", title)
					printed = true
				}
				arg, text := flag.UnquoteUsage(f)
				if arg != "" {
					arg = " " + arg
				}
				fmt.Fprintf(w, "  -%s%s\n    \t%s\n", f.Name, arg, text)
			}
		}
		group("flags", commonFlags)
		group("tuning flags (for experiments; the presets already pick good values)", tuningFlags)
	}
}

// parseArgs parses args into fs and checks that want positional arguments
// remain (want < 0: at least one). A flag after the file names is reported as
// such rather than as a wrong number of arguments. -h prints the help to
// standard output and returns flag.ErrHelp.
func parseArgs(fs *flag.FlagSet, args []string, want int, what string) error {
	fs.Init(fs.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return err
		}
		return usageError(err.Error())
	}
	for _, a := range fs.Args() {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && fs.Lookup(name) != nil {
			return usageError(fmt.Sprintf("flags go before the file names (found %s after them)", a))
		}
	}
	if (want >= 0 && fs.NArg() != want) || (want < 0 && fs.NArg() == 0) {
		return usageError(fmt.Sprintf("%s needs: %s", fs.Name(), what))
	}
	return nil
}

// codecFlags registers the tuning flags shared by compress and bench. The
// returned function builds the Options: the preset first, then only the flags
// the user actually gave, so a flag's default never undoes a preset.
func codecFlags(fs *flag.FlagSet) func() (codec.Options, error) {
	def := codec.DefaultOptions()
	preset := fs.String("preset", "default", "`NAME`: default (smallest output, slowest), fast (no fractal stage) or fastest (DEFLATE only)")
	width := fs.Int("width", 0, "lay the file out in rows of `N` bytes (0 = find the width automatically)")
	block := fs.Int("block", def.Fractal.Block, fmt.Sprintf("fractal stage: largest block it matches, `N` bytes per side (default %d)", def.Fractal.Block))
	iter := fs.Int("iter", def.Fractal.Iterations, fmt.Sprintf("fractal stage: `N` rounds the decoder runs to rebuild the data (default %d)", def.Fractal.Iterations))
	stride := fs.Int("stride", def.Fractal.Stride, fmt.Sprintf("fractal stage: spacing `N` of the positions it copies from (default %d; smaller searches more, slower)", def.Fractal.Stride))
	maxf := fs.Int("max-fractal", def.MaxFractalSize, "skip the fractal stage on inputs larger than `N` bytes (0 = no limit)")
	tile := fs.Int("tile", def.Fractal.Tile, "fractal stage: cut the data into independent square tiles of side `N` (0 = as large as -memory allows)")
	minb := fs.Int("min-block", def.Fractal.MinBlock, fmt.Sprintf("fractal stage: smallest block side `N`; blocks with no match are split down to this (default %d)", def.Fractal.MinBlock))
	workers := fs.Int("workers", 0, "use `N` CPU cores (0 = all)")
	seg := fs.Int("segment", 0, "slice size, `N` bytes, when each slice picks its own method (0 = the preset's: 64K, 256K fast, 1M fastest; -1 = off)")
	noCM := fs.Bool("no-cm", false, "do not use the context-mixing coder")
	maxCM := fs.Int("max-cm", def.MaxCMSize, "skip the context-mixing coder on pieces larger than `N` bytes (0 = 16 MiB)")
	attempts := fs.Int("cm-attempts", 0, "try `N` layouts with the context-mixing coder per piece (0 = automatic: 3, fewer on large pieces)")
	memory := fs.String("memory", "", "memory budget `SIZE`, such as 4G or 512M, that the compressor plans its work around (default 8G; an estimate, not a hard cap)")
	bigSeg := fs.String("big-segment", "", "cut very large inputs into independent pieces of `SIZE`, such as 64M (default: chosen from -memory)")
	noCopy := fs.Bool("no-copy", false, "fractal stage: do not use same-size block copies")
	noGate := fs.Bool("no-gate", false, "always run the fractal stage, even when a quick sample finds no repeated blocks")
	noRej := fs.Bool("no-reject", false, "try every method, even on data that looks incompressible")
	no := fs.Bool("no-fractal", false, "do not use the fractal stage")
	noPrep := fs.Bool("no-prep", false, "do not search for a row width or reshaping steps")
	video := fs.String("video", "", "the input is raw video with no header: `WxH[:FORMAT]` gives the frame size in pixels and the pixel format, one of "+codec.VideoFormats+". Turns off the fractal stage")
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
	verbose := fs.Bool("v", false, "print the sizes, the method chosen and the time to standard error")
	quiet := fs.Bool("q", false, "do not show progress (it is shown only when standard error is a terminal)")
	setUsage(fs, "[flags] IN OUT", `Compress IN into OUT. "-" reads standard input or writes standard output.
OUT is replaced if it exists. Flags go before the file names. The defaults
shown are those of -preset default.`)
	if err := parseArgs(fs, args, 2, "IN OUT"); err != nil {
		return err
	}
	opt, err := get()
	if err != nil {
		return usageError(err.Error())
	}
	if err := checkDistinct(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	in, size, cleanup, err := openInput(fs.Arg(0))
	if err != nil {
		return err
	}
	defer cleanup()
	start := time.Now()
	stop := func() {}
	if !*quiet && isTerminal(os.Stderr) {
		opt.Progress = new(codec.Progress)
		stop = showProgress(os.Stderr, opt.Progress, start)
	}
	defer stop()
	var rep codec.Report
	err = writeOutput(fs.Arg(1), func(w io.Writer) error {
		var err error
		rep, err = codec.CompressStream(w, in, size, opt)
		return err
	})
	if err != nil {
		return err
	}
	stop() // clear the progress line before the summary
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
	memory := fs.String("memory", "", "memory budget `SIZE`, such as 2G or 512M, to decode within: fewer pieces are decoded at once, and a file that cannot fit is refused (default: no limit)")
	setUsage(fs, "[flags] IN OUT", `Restore the original file from IN, a file written by "fpress compress", into
OUT. "-" reads standard input or writes standard output. OUT is replaced if it
exists; nothing is written if IN is damaged. Flags go before the file names.`)
	if err := parseArgs(fs, args, 2, "IN OUT"); err != nil {
		return err
	}
	var dopt codec.DecodeOptions
	if *memory != "" {
		n, err := parseSize(*memory)
		if err != nil {
			return usageError("-memory: " + err.Error())
		}
		dopt.MemoryLimit = n
	}
	if err := checkDistinct(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
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

// isTerminal reports whether f is a terminal (or another character device),
// where a progress line that rewrites itself makes sense.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Progress display: nothing for the first progressDelay (quick runs stay
// quiet), then one line, rewritten every progressEvery.
var (
	progressDelay = time.Second
	progressEvery = 500 * time.Millisecond
)

// showProgress draws p on w until the returned function is called; that
// function clears the line and may be called more than once.
func showProgress(w io.Writer, p *codec.Progress, start time.Time) func() {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		width := 0
		timer := time.NewTimer(progressDelay)
		defer timer.Stop()
		for {
			select {
			case <-quit:
				if width > 0 {
					fmt.Fprintf(w, "\r%*s\r", width, "")
				}
				return
			case <-timer.C:
				line := progressLine(p.Status(), time.Since(start))
				fmt.Fprintf(w, "\r%-*s", width, line)
				width = max(width, len(line))
				timer.Reset(progressEvery)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit); <-done }) }
}

// progressLine is one line of the progress display, such as
// "compressing: 37%, 6.1 of 16.8 MB, 31 s".
func progressLine(s codec.ProgressStatus, elapsed time.Duration) string {
	secs := fmt.Sprintf("%d s", int(elapsed.Seconds()))
	switch {
	case s.Checking && s.Done >= s.Total:
		return "checking the result, " + secs
	case s.Measured || s.Done > 0:
		pct := 100 * float64(s.Done) / float64(max(s.Total, 1))
		return fmt.Sprintf("compressing: %d%%, %s of %s, %s", int(pct), megabytes(s.Done), megabytes(s.Total), secs)
	}
	return fmt.Sprintf("compressing %s, %s", megabytes(s.Total), secs)
}

// megabytes formats n bytes as decimal megabytes, or gigabytes from 10 GB.
func megabytes(n int64) string {
	if n >= 10e9 {
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

// checkDistinct refuses an output that is the input itself (by any name, such
// as a link): the result would replace the original.
func checkDistinct(in, out string) error {
	if in == "-" || out == "-" {
		return nil
	}
	si, err := os.Stat(in)
	if err != nil {
		return nil // openInput reports it
	}
	if so, err := os.Stat(out); err == nil && os.SameFile(si, so) {
		return usageError(fmt.Sprintf("%s is both the input and the output; choose another output name", out))
	}
	return nil
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
// Something that is not a regular file, such as /dev/null or a named pipe,
// is written directly: it cannot be renamed over and has nothing to protect.
func writeOutput(path string, write func(io.Writer) error) error {
	direct := func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 1<<20)
		if err := write(bw); err != nil {
			return err
		}
		return bw.Flush()
	}
	if path == "-" {
		return direct(os.Stdout)
	}
	if st, err := os.Stat(path); err == nil && !st.Mode().IsRegular() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		if err := direct(f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
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
	head := fs.Int("head", 0, "use only the first `N` bytes of each file (0 = all)")
	ext := fs.Bool("ext", false, "add columns for xz -9e, zstd -19 and bzip2 -9, when they are installed")
	setUsage(fs, "[flags] FILE...", `Compress each file with every method and print one row per file: the size each
method gave ("-" = not tried), the one chosen, and the time. Nothing is
written. The README section "Reading bench output" explains the columns.`)
	if err := parseArgs(fs, args, -1, "at least one FILE"); err != nil {
		return err
	}
	opt, err := get()
	if err != nil {
		return usageError(err.Error())
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
