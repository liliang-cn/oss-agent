// Package cite renders source citations for grounded answers. It is shared by the
// agent (which tells the model how to cite), the HTTP layer (which appends a
// deterministic Sources footer when the model doesn't), and the eval harness
// (which measures citation coverage).
package cite

import "strings"

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

// Footer renders a markdown "Sources" section mapping a bracket label to each
// source, or "" when there are no sources or the answer already lists its own.
func Footer(answer string, sources []string) string {
	if len(sources) == 0 || hasOwnSources(answer) {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n**Sources**\n")
	for _, s := range sources {
		b.WriteString("- [" + Label(s) + "] " + s + "\n")
	}
	return b.String()
}
