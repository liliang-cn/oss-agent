package ingest

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// CLI help ingestion.
//
// A knowledge base built from a repository knows the product's architecture
// (from the code graph) and its operations (from runbooks), and neither records
// how to spell a flag. Asked for a runnable command, an agent therefore
// assembles one from the shape of the others — plausible subcommand order,
// plausible flag names — and it is indistinguishable from a correct command
// until an operator pastes it. Observed on a deployed copilot: it produced
// `gateway create nfs --vip …` where the binary wants
// `gateway nfs create --service-ip …`, with three of four flags invented.
//
// The binary's own --help is the one source that cannot drift from the binary,
// so it is what gets ingested. This walks every subcommand and renders one
// document.

// helpTimeout bounds a single --help invocation. A CLI that hangs here is
// broken in a way the caller needs to hear about rather than wait through.
const helpTimeout = 20 * time.Second

// maxHelpCommands bounds the walk. A tree this large is either a runaway
// recursion or a CLI whose help is not the right thing to ingest wholesale.
const maxHelpCommands = 2000

// CLIHelpStats summarizes a walk.
type CLIHelpStats struct {
	Commands int
	Failed   []string
}

// CLIHelp walks every subcommand of bin, collecting `--help` output into a
// single markdown document.
//
// The walk reads subcommand names from the "Available Commands:" block that
// cobra, clap and most argparse wrappers emit. A CLI that formats its help
// differently yields just the top-level page, which is still better than
// nothing and never wrong.
func CLIHelp(ctx context.Context, bin string, args ...string) (string, CLIHelpStats, error) {
	name := bin
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}

	var st CLIHelpStats
	seen := map[string]bool{}
	sections := make([]string, 0, 32)

	var walk func(path []string) error
	walk = func(path []string) error {
		key := strings.Join(path, " ")
		// A subcommand reachable by two routes would otherwise be emitted
		// twice, and a duplicated chunk is a duplicated retrieval candidate.
		if seen[key] {
			return nil
		}
		seen[key] = true
		if st.Commands >= maxHelpCommands {
			return fmt.Errorf("stopped after %d commands: %q has an implausibly large command tree", maxHelpCommands, name)
		}

		out, err := runHelp(ctx, bin, append(append([]string(nil), args...), path...))
		if err != nil {
			st.Failed = append(st.Failed, key)
			return nil // one unreachable subcommand must not lose the rest
		}
		if strings.TrimSpace(out) == "" {
			return nil
		}
		st.Commands++

		title := name
		if key != "" {
			title += " " + key
		}
		sections = append(sections, "### "+title+"\n\n```\n"+strings.TrimRight(out, "\n")+"\n```\n")

		for _, sub := range subcommandNames(out) {
			if err := walk(append(append([]string(nil), path...), sub)); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(nil); err != nil {
		return "", st, err
	}
	if st.Commands == 0 {
		return "", st, fmt.Errorf("%s produced no help output — is it the right binary?", bin)
	}
	sort.Strings(st.Failed)
	return cliHelpHeader(name) + "\n" + strings.Join(sections, "\n"), st, nil
}

func cliHelpHeader(name string) string {
	return fmt.Sprintf(`# %s command reference (generated from --help)

This is the authoritative syntax for the %s command line: the order of
subcommands, the exact spelling of every flag, which are required, and what the
defaults are. It comes from the binary's own --help output, not from prose about
it, so it cannot drift from the binary.

When answering anything that involves running %s, take the command from here
verbatim. Do not adapt a flag from memory or from another command's shape — a
command that is wrong in one flag looks exactly as authoritative as one that
works, and only fails once an operator has run it.
`, name, name, name)
}

// subcommandNames reads the names listed under "Available Commands:".
//
// help and completion are skipped: cobra generates them for every command, they
// document the help system rather than the product, and completion carries one
// near-identical page per shell.
func subcommandNames(help string) []string {
	var names []string
	inBlock := false
	sc := bufio.NewScanner(strings.NewReader(help))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Available Commands:"), strings.HasPrefix(line, "Commands:"),
			strings.HasPrefix(line, "SUBCOMMANDS:"):
			inBlock = true
			continue
		case inBlock && isHelpSectionHeader(line):
			inBlock = false
			continue
		}
		if !inBlock || strings.TrimSpace(line) == "" {
			continue
		}
		// Entries are indented "  name   description"; a non-indented line ends
		// the block even without a recognised header.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inBlock = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sub := fields[0]
		if sub == "help" || sub == "completion" || strings.HasPrefix(sub, "-") {
			continue
		}
		names = append(names, sub)
	}
	return names
}

func isHelpSectionHeader(line string) bool {
	for _, h := range []string{"Flags:", "Global Flags:", "Additional", "Use \"", "Options:", "ARGS:", "OPTIONS:"} {
		if strings.HasPrefix(line, h) {
			return true
		}
	}
	return false
}

// runHelp invokes one --help. Both streams are kept: cobra writes help to
// stdout, several other frameworks write it to stderr, and a caller cannot tell
// which convention a binary follows without looking.
func runHelp(ctx context.Context, bin string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, helpTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, append(argv, "--help")...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := stdout.String() + stderr.String()
	// A non-zero exit is normal for --help on some frameworks, so the output is
	// what decides success; only a genuinely empty result is a failure.
	if strings.TrimSpace(out) == "" && err != nil {
		return "", err
	}
	return out, nil
}
