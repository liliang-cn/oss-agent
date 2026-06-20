package scaffold

import (
	"fmt"
	"strings"
)

// render turns a draft into a domain.toml string, matching the layout of
// examples/linbit/domain.toml. red_lines and probes are annotated as REVIEW so
// the operator audits the safety wall before trusting it.
func render(d *draft, repoName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# domain.toml drafted by `oss-agent init` for %q.\n", repoName)
	b.WriteString("# REVIEW everything below — especially [[red_lines]] (the destructive-command\n")
	b.WriteString("# safety wall) and [[probes]] (commands the agent may run). These were proposed\n")
	b.WriteString("# by an LLM from the repo and MUST be verified against the product's real CLI.\n\n")

	name := d.Name
	if name == "" {
		name = repoName
	}
	title := d.Title
	if title == "" {
		title = name
	}
	fmt.Fprintf(&b, "name = %s\n", basicStr(name))
	fmt.Fprintf(&b, "title = %s\n\n", basicStr(title))

	persona := strings.ReplaceAll(d.Persona, "'''", "''")
	fmt.Fprintf(&b, "persona = '''\n%s\n'''\n\n", strings.TrimSpace(persona))

	writeStrArray(&b, "entity_types", d.EntityTypes)
	b.WriteString("\n")
	writeStrArray(&b, "relation_types", d.RelationTypes)
	b.WriteString("\n")

	b.WriteString("# Regex sources (group 1 = message) for log triage.\n")
	if len(d.ErrorPatterns) == 0 {
		b.WriteString("error_patterns = []\n\n")
	} else {
		b.WriteString("error_patterns = [\n")
		for _, p := range d.ErrorPatterns {
			fmt.Fprintf(&b, "  %s,\n", regexStr(p))
		}
		b.WriteString("]\n\n")
	}

	b.WriteString("# REVIEW: destructive-command blocks. Drop/add rules to match this product.\n")
	for _, r := range d.RedLines {
		b.WriteString("[[red_lines]]\n")
		fmt.Fprintf(&b, "id = %s\n", basicStr(orDefault(r.ID, "unnamed-rule")))
		fmt.Fprintf(&b, "severity = %s\n", basicStr(orDefault(r.Severity, "CRITICAL")))
		fmt.Fprintf(&b, "pattern = %s\n", regexStr(r.Pattern))
		fmt.Fprintf(&b, "reason = %s\n\n", basicStr(r.Reason))
	}

	b.WriteString("# REVIEW: read-only diagnostic probes. Confirm each argv is truly read-only.\n")
	for _, p := range d.Probes {
		b.WriteString("[[probes]]\n")
		fmt.Fprintf(&b, "name = %s\n", basicStr(p.Name))
		fmt.Fprintf(&b, "description = %s\n", basicStr(p.Description))
		fmt.Fprintf(&b, "argv = %s\n\n", strArrayInline(p.Argv))
	}
	return b.String()
}

func writeStrArray(b *strings.Builder, key string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(b, "%s = []\n", key)
		return
	}
	fmt.Fprintf(b, "%s = [\n", key)
	for _, it := range items {
		fmt.Fprintf(b, "  %s,\n", basicStr(it))
	}
	b.WriteString("]\n")
}

func strArrayInline(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = basicStr(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// basicStr renders a TOML basic (double-quoted) string with escaping.
func basicStr(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// regexStr prefers a TOML literal string (”) so backslashes survive verbatim; if
// the pattern contains a single quote (which literal strings can't escape), it
// falls back to a basic string.
func regexStr(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return basicStr(s)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
