// Package cite renders source citations for grounded answers. It is shared by the
// agent (which tells the model how to cite), the HTTP layer (which appends a
// deterministic Sources footer when the model doesn't), and the eval harness
// (which measures citation coverage).
package cite

import (
	"encoding/json"
	"strings"
)

// CollectSources pulls the "source" of every hit out of a knowledge_search tool
// result into a deduped, order-preserving list. It round-trips through JSON so it
// works regardless of the concrete Go shape the event carries (map, typed slice,
// or a JSON string).
func CollectSources(res any, seen map[string]bool, out *[]string) {
	var b []byte
	if s, ok := res.(string); ok {
		b = []byte(s)
	} else {
		var err error
		if b, err = json.Marshal(res); err != nil {
			return
		}
	}
	var parsed struct {
		Hits []struct {
			Source string `json:"source"`
		} `json:"hits"`
	}
	if json.Unmarshal(b, &parsed) != nil {
		return
	}
	for _, h := range parsed.Hits {
		if h.Source == "" || seen[h.Source] {
			continue
		}
		seen[h.Source] = true
		*out = append(*out, h.Source)
	}
}

// Label turns a source / document id into a short, stable, bracket-safe citation
// label (letters, digits, hyphen, underscore only) so it reads cleanly inline as
// [label] and matches the common \[[A-Za-z0-9][\w-]*\] citation pattern.
//
//	ua:linstor-server                                  -> linstor-server
//	linbit-documentation/UG9/en/drbd-troubleshooting.adoc -> drbd-troubleshooting
func Label(source string) string {
	s := strings.TrimPrefix(source, "ua:")
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, '.'); i > 0 { // strip a file extension
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "src"
	}
	return out
}

// hasOwnSources reports whether the answer already contains its own Sources section.
func hasOwnSources(answer string) bool {
	return strings.Contains(strings.ToLower(answer), "sources") &&
		(strings.Contains(answer, "**Sources") || strings.Contains(answer, "# Sources") || strings.Contains(answer, "## Sources"))
}

// Footer renders a markdown "Sources" section for the sources the answer
// actually cites, or "" when it cites none or already lists its own.
//
// It used to list everything retrieved this turn, on the reasoning that a model
// which forgot to cite should still show its working. That inverts into a lie
// as soon as retrieval returns something the answer did not use — and retrieval
// always returns something, because the relevance gate is query-relative and
// keeps the best chunk however far off topic it is. Asking about a parameter
// the corpus has never heard of produced a confident answer from the model's
// own memory with the nearest unrelated document appended underneath it,
// formatted as a source. An answer that reads as grounded and is not is worse
// than one that admits it is unsourced, because nobody re-checks a citation.
//
// The cost is the case this was built for: a source the model used but did not
// label goes unlisted. That is a loss of convenience. The other way round is a
// loss of truth.
func Footer(answer string, sources []string) string {
	if len(sources) == 0 || hasOwnSources(answer) {
		return ""
	}
	var used []string
	for _, s := range sources {
		if strings.Contains(answer, "["+Label(s)+"]") {
			used = append(used, s)
		}
	}
	if len(used) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n**Sources**\n")
	for _, s := range used {
		b.WriteString("- [" + Label(s) + "] " + s + "\n")
	}
	return b.String()
}
