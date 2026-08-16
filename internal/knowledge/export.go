package knowledge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Export reassembles stored documents back into markdown files.
//
// Not every document in a knowledge base came from a file. Notes written
// straight into the store through the UI or the API — the ones recording what
// an incident actually turned out to be, which are usually the most valuable
// documents in it — exist only as chunks in a table. A store audited after a
// year held seven such notes and no copy of any of them: rebuilding the index
// would have destroyed knowledge that had no other home.
//
// Chunks are concatenated in index order. Chunking overlaps by design, so the
// seam between two chunks is de-duplicated on the way out; the result is close
// to the original text rather than byte-identical to it.

// ExportStats summarizes a run.
type ExportStats struct {
	Documents int
	Files     []string
	// Skipped names documents written out under a shared filename because
	// their ids differ only in characters a path cannot hold.
	Skipped []string
}

// Export writes every document (optionally only those whose id has the given
// prefix) to dir as one markdown file each.
func (s *Store) Export(ctx context.Context, dir, prefix string) (*ExportStats, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	query := `
		SELECT json_extract(metadata,'$.document_id') AS doc, id, content
		FROM embeddings
		WHERE doc IS NOT NULL AND doc <> ''`
	args := []any{}
	if prefix != "" {
		query += ` AND doc LIKE ?`
		args = append(args, prefix+"%")
	}
	// Ordered by id because chunk ids carry their index ("doc.md#3"), so this
	// is the order the text was split in.
	query += ` ORDER BY doc, id`

	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	docs := map[string][]string{}
	var order []string
	for rows.Next() {
		var doc, id, content string
		if err := rows.Scan(&doc, &id, &content); err != nil {
			return nil, err
		}
		if _, seen := docs[doc]; !seen {
			order = append(order, doc)
		}
		docs[doc] = append(docs[doc], content)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	st := &ExportStats{}
	usedNames := map[string]string{}
	for _, doc := range order {
		name := exportFilename(doc)
		if owner, taken := usedNames[name]; taken && owner != doc {
			// Reported rather than silently overwritten: two ids colliding on a
			// filename means one of them would have been lost.
			st.Skipped = append(st.Skipped, doc)
			continue
		}
		usedNames[name] = doc

		path := filepath.Join(dir, name)
		body := "<!-- exported from the knowledge base; document_id: " + doc + " -->\n\n" +
			joinChunks(docs[doc]) + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		st.Documents++
		st.Files = append(st.Files, path)
	}
	sort.Strings(st.Skipped)
	return st, nil
}

// exportFilename turns a document id into one path segment. Ids routinely carry
// slashes ("corpus/user-guide.md") and colons ("ua:repo:graph"), neither of
// which may become a directory here — a flat directory is what re-ingests.
func exportFilename(doc string) string {
	name := strings.NewReplacer("/", "_", ":", "_", string(os.PathSeparator), "_").Replace(doc)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "untitled"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".md") {
		name += ".md"
	}
	return name
}

// joinChunks concatenates chunks, dropping the overlap the chunker introduced.
//
// Overlap exists so a sentence split across a boundary is still retrievable
// whole; written back out verbatim it would duplicate a paragraph at every
// seam.
func joinChunks(chunks []string) string {
	if len(chunks) == 0 {
		return ""
	}
	out := chunks[0]
	for _, next := range chunks[1:] {
		out += stripOverlap(out, next)
	}
	return out
}

// stripOverlap returns next without whatever suffix of prev it repeats.
func stripOverlap(prev, next string) string {
	const maxOverlap = 2000
	limit := len(prev)
	if limit > maxOverlap {
		limit = maxOverlap
	}
	if limit > len(next) {
		limit = len(next)
	}
	// Longest match first: a short accidental match (a repeated newline, a
	// common word) would otherwise win and leave the real overlap in place.
	for n := limit; n > 20; n-- {
		if strings.HasSuffix(prev, next[:n]) {
			return next[n:]
		}
	}
	return next
}
