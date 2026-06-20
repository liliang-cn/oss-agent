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
var codeExts = map[string]bool{".c": true, ".h": true, ".cpp": true, ".cc": true, ".java": true}
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "build": true, "target": true, ".github": true}

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

		// source file → mine error/log strings
		st.CodeFiles++
		msgs := extractErrorStrings(string(b), dom.ErrorPatterns)
		if len(msgs) == 0 {
			return nil
		}
		content := fmt.Sprintf("Error and log messages emitted by %s (%s):\n\n- %s",
			rel, repoName, strings.Join(msgs, "\n- "))
		if e := store.IngestSemantic(ctx, id+"#errors", repoName+": "+rel+" (error strings)", content, ex); e != nil {
			return fmt.Errorf("ingest error strings %s: %w", rel, e)
		}
		st.ErrorStrings += len(msgs)
		return nil
	})
	return st, err
}

// extractErrorStrings returns the de-duplicated message literals in src.
func extractErrorStrings(src string, patterns []*regexp.Regexp) []string {
	seen := map[string]bool{}
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			msg := strings.TrimSpace(m[1])
			if len(msg) >= 8 && !seen[msg] {
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
