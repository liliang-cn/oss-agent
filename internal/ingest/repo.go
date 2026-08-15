// Package ingest walks a project's source/doc repository and feeds it into the
// knowledge base. Docs are ingested as prose; source files are mined for the
// log/error string literals operators actually see (printk, dev_err, LOG.error,
// thrown exceptions) so a real-world error line can be traced back to its source.
package ingest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/extract"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
)

// Stats summarizes an ingest run.
type Stats struct {
	DocFiles     int
	CodeFiles    int
	ErrorStrings int
	Skipped      int
}

var docExts = map[string]bool{".md": true, ".adoc": true, ".asciidoc": true, ".rst": true, ".txt": true}

// codeExts are the sources worth mining for the message literals an operator
// actually sees in a log. The list was C and Java only, which silently excluded
// every Go project — including domains whose own error_patterns name Go
// constructs (zap, logger, fmt.Errorf). A pattern that can never reach a file
// is a pattern that does nothing.
var codeExts = map[string]bool{
	".c": true, ".h": true, ".cpp": true, ".cc": true, ".java": true,
	".go": true, ".rs": true, ".py": true, ".ts": true, ".js": true,
}

// skipDirs are directories that hold no operator-facing knowledge. ".claude"
// earns its place the hard way: its worktrees/ can hold whole duplicate copies
// of the repository — one audited ingest was 72% worktree copies by file count
// — and every duplicate chunk is a duplicate retrieval candidate.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "build": true,
	"target": true, ".github": true, ".claude": true, ".understand-anything": true,
}

// skipFile drops files whose contents are not knowledge: generated code
// restates the .proto that generated it, thousands of lines at a time, and a
// test's fmt.Errorf is an assertion message no operator will ever see in a log.
func skipFile(name string) bool {
	return strings.HasSuffix(name, ".pb.go") ||
		strings.HasSuffix(name, ".pb.gw.go") ||
		strings.HasSuffix(name, "_test.go") ||
		strings.HasSuffix(name, ".min.js")
}

const maxFileBytes = 512 * 1024 // skip very large files

// Repo ingests a local repository directory into the store, using the domain's
// error-string patterns for code and (when ex != nil) the LLM ontology extractor.
func Repo(ctx context.Context, store *knowledge.Store, root, repoName string, dom *domain.Domain, ex *extract.Extractor) (Stats, error) {
	var st Stats
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFile(d.Name()) {
			st.Skipped++
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		isDoc, isCode := docExts[ext], codeExts[ext]
		if !isDoc && !isCode {
			st.Skipped++
			return nil
		}
		info, _ := d.Info()
		if info != nil && info.Size() > maxFileBytes {
			st.Skipped++
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil || strings.TrimSpace(string(b)) == "" {
			st.Skipped++
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		id := repoName + "/" + filepath.ToSlash(rel)

		if isDoc {
			if e := store.IngestSemantic(ctx, id, repoName+": "+rel, string(b), ex); e != nil {
				return fmt.Errorf("ingest doc %s: %w", rel, e)
			}
			st.DocFiles++
			return nil
		}

		// Source file → mine each declared group separately. One document per
		// group, not one per file: a question about types should retrieve types
		// rather than compete for the same chunk with a list of log strings.
		st.CodeFiles++
		for _, g := range dom.SourceGroups() {
			vals := extractMatches(string(b), g.Compiled, g.MinLength)
			if len(vals) == 0 {
				continue
			}
			content := fmt.Sprintf("%s %s (%s):\n\n- %s",
				g.Summary, rel, repoName, strings.Join(vals, "\n- "))
			docID := id + "#" + g.Name
			title := fmt.Sprintf("%s: %s (%s)", repoName, rel, g.Name)
			if e := store.IngestSemantic(ctx, docID, title, content, ex); e != nil {
				return fmt.Errorf("ingest %s of %s: %w", g.Name, rel, e)
			}
			st.ErrorStrings += len(vals)
		}
		return nil
	})
	return st, err
}

// extractMatches returns the de-duplicated group-1 captures in src, dropping
// anything shorter than minLen.
func extractMatches(src string, patterns []*regexp.Regexp, minLen int) []string {
	if minLen <= 0 {
		minLen = 3
	}
	seen := map[string]bool{}
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			if len(m) < 2 {
				continue
			}
			msg := strings.TrimSpace(m[1])
			if len(msg) >= minLen && !seen[msg] {
				seen[msg] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
