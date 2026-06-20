// Package loganalyze is a product-agnostic log triage engine. It accepts a single
// log file, a directory, or an archive (.tar.gz/.tgz/.tar/.zip/.gz), walks every
// text file inside, and extracts error/warning findings using (a) universal log
// severity signals and (b) the active domain's error_patterns (from domain.toml).
// Findings are normalized into signatures and aggregated so the noisy bundle
// collapses into a short ranked list of distinct problems.
//
// Nothing here is specific to any product: a service log, a kernel log, or a plain
// app log all go through the same path. Product tuning enters only via the domain's regexes.
package loganalyze

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Finding is one detected problem occurrence.
type Finding struct {
	File      string   // path relative to the analyzed root
	Line      int      // 1-based line number of the trigger line
	Severity  string   // FATAL | ERROR | WARN
	Signature string   // normalized message used for de-duplication
	Message   string   // the trigger line (trimmed)
	Context   []string // following backtrace/continuation lines (best-effort)
	Source    string   // which detector fired: "domain" or "generic"
}

// Group is a set of findings sharing a signature (the same underlying problem).
type Group struct {
	Signature string
	Severity  string
	Count     int
	Files     []string  // distinct files where it occurred
	Sample    Finding   // a representative occurrence (with context)
}

// Report is the result of analyzing a source.
type Report struct {
	Root          string
	FilesTotal    int
	FilesScanned  int
	FilesSkipped  int // binary/oversized/unreadable
	Findings      int
	Groups        []Group // ranked: FATAL first, then by count
}

const (
	maxFileBytes = 64 << 20 // skip files larger than 64 MiB
	maxContext   = 8        // backtrace lines captured after a trigger
	binarySniff  = 8000     // bytes inspected for NUL to detect binary
)

// skipExt are extensions never treated as text logs.
var skipExt = map[string]bool{
	".db": true, ".mv.db": true, ".sqlite": true, ".png": true, ".jpg": true,
	".jpeg": true, ".gif": true, ".pdf": true, ".zip": true, ".gz": true,
	".bz2": true, ".xz": true, ".tar": true, ".tgz": true, ".so": true,
	".o": true, ".a": true, ".class": true, ".jar": true, ".bin": true,
	".img": true, ".iso": true, ".pyc": true, ".woff": true, ".woff2": true,
	".ico": true, ".svg": true,
}

// ── generic, product-independent detectors ──
//
// These encode universal log semantics (not a product). Group 1, when present,
// is treated as the human message; otherwise the whole match is the message.
var (
	reFatal = regexp.MustCompile(`(?i)\b(FATAL|CRITICAL|EMERG|ALERT|panic:|kernel BUG|general protection fault|segfault|oom-killer|Out of memory)\b`)
	reError = regexp.MustCompile(`(?i)(\bERROR\b|\bSEVERE\b|\bfailed\b|\bfailure\b|\bcannot\b|\bunable to\b|[A-Za-z_][A-Za-z0-9_.]*(?:Exception|Error)\b|Traceback \(most recent call last\))`)
	reWarn  = regexp.MustCompile(`(?i)\b(WARN|WARNING)\b`)

	// reNoise matches generic structured-report scaffolding — banner lines, field
	// labels, and separators that merely contain the word "error" but carry no
	// distinct problem. This is log-format boilerplate, not product logic; the
	// substantive "...message: <text>" / inline-exception lines are kept.
	reNoise = regexp.MustCompile(`(?i)^\s*(` +
		`[=\-_*]{3,}` + // separator rules
		`|(end of |begin(ning)? of )?error report\b` +
		`|reported error:\s*$` +
		`|category:\s*\w+\s*$` +
		`|class( canonical)? name:` +
		`|(error|reported) time:` +
		`|application:|module:|build (id|time):|version:\s` +
		`)`)

	// continuation/backtrace lines worth capturing as context
	reCont = regexp.MustCompile(`^\s+|^\s*at\s|\.java:\d+|File ".*", line \d+|^\s*Method\b|0x[0-9a-fA-F]{4,}|^Caused by:|^\t`)

	// normalization: collapse volatile tokens so occurrences dedup cleanly
	reTimestamp = regexp.MustCompile(`\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reSyslogTS  = regexp.MustCompile(`(?i)^[a-z]{3}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}`)
	reHex       = regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{6,}\b`)
	reUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reNum       = regexp.MustCompile(`\b\d+\b`)
	reWS        = regexp.MustCompile(`\s+`)
)

// Resolve turns a path into a walkable root, extracting archives to a temp dir.
// The returned cleanup removes any temp dir (no-op for plain files/dirs).
func Resolve(path string) (root string, cleanup func(), error error) {
	noop := func() {}
	info, err := os.Stat(path)
	if err != nil {
		return "", noop, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return path, noop, nil
	}
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar"):
		dir, err := os.MkdirTemp("", "oss-log-*")
		if err != nil {
			return "", noop, err
		}
		if err := extractTar(path, dir); err != nil {
			os.RemoveAll(dir)
			return "", noop, err
		}
		return dir, func() { os.RemoveAll(dir) }, nil
	case strings.HasSuffix(lower, ".zip"):
		dir, err := os.MkdirTemp("", "oss-log-*")
		if err != nil {
			return "", noop, err
		}
		if err := extractZip(path, dir); err != nil {
			os.RemoveAll(dir)
			return "", noop, err
		}
		return dir, func() { os.RemoveAll(dir) }, nil
	case strings.HasSuffix(lower, ".gz"):
		dir, err := os.MkdirTemp("", "oss-log-*")
		if err != nil {
			return "", noop, err
		}
		out := filepath.Join(dir, strings.TrimSuffix(filepath.Base(path), ".gz"))
		if err := gunzipTo(path, out); err != nil {
			os.RemoveAll(dir)
			return "", noop, err
		}
		return dir, func() { os.RemoveAll(dir) }, nil
	default:
		return path, noop, nil // a single plain file
	}
}

// Analyze walks root and returns a ranked report. domainPatterns are the active
// domain's compiled error_patterns (may be empty); they augment generic detection.
func Analyze(root string, domainPatterns []*regexp.Regexp) (*Report, error) {
	rep := &Report{Root: root}
	bySig := map[string]*Group{}

	walk := func(path string, isDir bool, size int64) {
		if isDir {
			return
		}
		rep.FilesTotal++
		rel, _ := filepath.Rel(root, path)
		if rel == "" || rel == "." {
			rel = filepath.Base(path)
		}
		if !looksTextual(path, size) {
			rep.FilesSkipped++
			return
		}
		findings, ok := scanFile(path, rel, domainPatterns)
		if !ok {
			rep.FilesSkipped++
			return
		}
		rep.FilesScanned++
		for _, f := range findings {
			rep.Findings++
			g := bySig[f.Signature]
			if g == nil {
				g = &Group{Signature: f.Signature, Severity: f.Severity, Sample: f}
				bySig[f.Signature] = g
			}
			g.Count++
			if sevRank(f.Severity) < sevRank(g.Severity) {
				g.Severity = f.Severity // keep the most severe label
			}
			if len(f.Context) > len(g.Sample.Context) {
				g.Sample = f // prefer the richest sample (with backtrace)
			}
			if !contains(g.Files, f.File) {
				g.Files = append(g.Files, f.File)
			}
		}
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		err = filepath.Walk(root, func(p string, fi os.FileInfo, e error) error {
			if e != nil {
				return nil // tolerate unreadable entries
			}
			walk(p, fi.IsDir(), fi.Size())
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		walk(root, false, info.Size())
	}

	for _, g := range bySig {
		rep.Groups = append(rep.Groups, *g)
	}
	// Rank by a relevance score, not raw count: a hard crash with a backtrace must
	// outrank a one-line service-flap repeated thousands of times. Count still
	// counts, but log-scaled so sheer repetition can't dominate.
	sort.Slice(rep.Groups, func(i, j int) bool {
		si, sj := score(rep.Groups[i]), score(rep.Groups[j])
		if si != sj {
			return si > sj
		}
		return rep.Groups[i].Signature < rep.Groups[j].Signature
	})
	return rep, nil
}

// score ranks a group: severity dominates, an attached backtrace marks a real
// exception/crash, spread across files matters, and count contributes on a log
// scale so a 15000× flap doesn't bury a single critical stack trace.
func score(g Group) float64 {
	s := 0.0
	switch g.Severity {
	case "FATAL":
		s += 1000
	case "ERROR":
		s += 100
	case "WARN":
		s += 10
	}
	if len(g.Sample.Context) > 0 { // has a backtrace → a real exception, not log chatter
		s += 60 + float64(len(g.Sample.Context))*3
	}
	if g.Sample.Source == "domain" { // matched a product-specific error_pattern
		s += 40
	}
	s += float64(len(g.Files)) * 5
	// log-scaled count: 1→0, 10→~23, 100→~46, 15000→~96
	if g.Count > 1 {
		s += 10 * math.Log(float64(g.Count))
	}
	return s
}

// scanFile reads a text file and extracts findings. Returns ok=false if the file
// can't be read or sniffs as binary.
func scanFile(path, rel string, domainPatterns []*regexp.Regexp) ([]Finding, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	// binary sniff
	head := make([]byte, binarySniff)
	n, _ := io.ReadFull(f, head)
	if containsNUL(head[:n]) {
		return nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}

	var out []Finding
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	lines := make([]string, 0, 1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for i, line := range lines {
		sev, msg, src := classify(line, domainPatterns)
		if sev == "" {
			continue
		}
		fnd := Finding{
			File: rel, Line: i + 1, Severity: sev,
			Message: strings.TrimSpace(msg), Source: src,
			Signature: signature(msg),
		}
		// capture following continuation/backtrace lines as context
		for j := i + 1; j < len(lines) && j <= i+maxContext; j++ {
			if strings.TrimSpace(lines[j]) == "" {
				break
			}
			if reCont.MatchString(lines[j]) {
				fnd.Context = append(fnd.Context, strings.TrimRight(lines[j], " \t"))
				continue
			}
			break
		}
		out = append(out, fnd)
	}
	return out, true
}

// classify returns the severity, message, and detector source for a line, or an
// empty severity if the line is not a finding. Domain patterns take precedence.
func classify(line string, domainPatterns []*regexp.Regexp) (sev, msg, src string) {
	for _, re := range domainPatterns {
		if m := re.FindStringSubmatch(line); m != nil {
			text := line
			if len(m) > 1 && m[1] != "" {
				text = m[1]
			}
			return "ERROR", text, "domain"
		}
	}
	if reNoise.MatchString(line) {
		return "", "", "" // structured-report scaffolding, not a distinct problem
	}
	if m := reFatal.FindString(line); m != "" {
		return "FATAL", line, "generic"
	}
	if m := reError.FindString(line); m != "" {
		return "ERROR", line, "generic"
	}
	if m := reWarn.FindString(line); m != "" {
		return "WARN", line, "generic"
	}
	return "", "", ""
}

// signature normalizes a message so distinct occurrences of the same problem
// collapse to one group: timestamps, hex/uuid ids, and numbers are masked.
func signature(msg string) string {
	s := strings.TrimSpace(msg)
	s = reSyslogTS.ReplaceAllString(s, "")
	s = reTimestamp.ReplaceAllString(s, "<ts>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reNum.ReplaceAllString(s, "<n>")
	s = reWS.ReplaceAllString(s, " ")
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		s = strings.ToLower(strings.TrimSpace(msg))
	}
	return s
}

// looksTextual is a cheap pre-filter by extension and size (content sniff happens
// in scanFile).
func looksTextual(path string, size int64) bool {
	if size > maxFileBytes {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	return !skipExt[ext]
}

func extractTar(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(strings.ToLower(src), ".gz") || strings.HasSuffix(strings.ToLower(src), ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := writeMember(dst, hdr.Name, hdr.FileInfo().IsDir(), hdr.FileInfo().Mode(), tr); err != nil {
			return err
		}
	}
}

func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeMember(dst, zf.Name, zf.FileInfo().IsDir(), zf.Mode(), rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeMember safely writes one archive member under dst, guarding against path
// traversal (zip-slip).
func writeMember(dst, name string, isDir bool, mode os.FileMode, r io.Reader) error {
	target := filepath.Join(dst, name)
	if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) && target != filepath.Clean(dst) {
		return fmt.Errorf("unsafe path in archive: %s", name)
	}
	if isDir {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, r)
	return err
}

func gunzipTo(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, gz)
	return err
}

// Render returns a human-readable triage report (top n groups).
func (r *Report) Render(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scanned %d/%d files (%d skipped) — %d findings in %d distinct problems.\n",
		r.FilesScanned, r.FilesTotal, r.FilesSkipped, r.Findings, len(r.Groups))
	if len(r.Groups) == 0 {
		b.WriteString("No errors or warnings detected.\n")
		return b.String()
	}
	if n <= 0 || n > len(r.Groups) {
		n = len(r.Groups)
	}
	for i := 0; i < n; i++ {
		g := r.Groups[i]
		fmt.Fprintf(&b, "\n%d. [%s ×%d] %s\n", i+1, g.Severity, g.Count, oneLine(g.Sample.Message, 160))
		files := g.Files
		if len(files) > 4 {
			files = append(append([]string{}, files[:4]...), fmt.Sprintf("(+%d more)", len(g.Files)-4))
		}
		fmt.Fprintf(&b, "   files: %s\n", strings.Join(files, ", "))
		fmt.Fprintf(&b, "   first: %s:%d\n", g.Sample.File, g.Sample.Line)
		for _, c := range g.Sample.Context {
			fmt.Fprintf(&b, "     | %s\n", oneLine(c, 160))
		}
	}
	return b.String()
}

// Brief returns a compact, token-friendly digest of the top n problems for an LLM
// diagnosis prompt (severity, count, message, a little backtrace, locations).
func (r *Report) Brief(n int) string {
	if n <= 0 || n > len(r.Groups) {
		n = len(r.Groups)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Log triage digest — %d findings across %d distinct problems in %d files.\n",
		r.Findings, len(r.Groups), r.FilesScanned)
	for i := 0; i < n; i++ {
		g := r.Groups[i]
		fmt.Fprintf(&b, "\n[%d] severity=%s occurrences=%d files=%d\n", i+1, g.Severity, g.Count, len(g.Files))
		fmt.Fprintf(&b, "    message: %s\n", oneLine(g.Sample.Message, 240))
		for _, c := range g.Sample.Context {
			if t := strings.TrimSpace(c); t != "" {
				fmt.Fprintf(&b, "    | %s\n", oneLine(t, 200))
			}
		}
		loc := g.Files
		if len(loc) > 3 {
			loc = loc[:3]
		}
		fmt.Fprintf(&b, "    at: %s\n", strings.Join(loc, ", "))
	}
	return b.String()
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(reWS.ReplaceAllString(s, " "))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func sevRank(s string) int {
	switch s {
	case "FATAL":
		return 0
	case "ERROR":
		return 1
	case "WARN":
		return 2
	}
	return 3
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
