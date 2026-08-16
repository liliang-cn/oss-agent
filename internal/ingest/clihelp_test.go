package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI writes a script that answers --help like cobra does, with a nested
// subcommand tree, so the walk can be exercised without a real binary.
func fakeCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	script := `#!/bin/sh
case "$*" in
  "--help")
    echo "Fake CLI"
    echo ""
    echo "Available Commands:"
    echo "  gateway     Gateway management"
    echo "  help        Help about any command"
    echo "  completion  Generate completions"
    echo ""
    echo "Flags:"
    echo "  -h, --help   help for fake"
    ;;
  "gateway --help")
    echo "Gateway management"
    echo ""
    echo "Available Commands:"
    echo "  nfs   NFS gateway"
    echo ""
    echo "Flags:"
    echo "  -h, --help   help for gateway"
    ;;
  "gateway nfs --help")
    echo "NFS gateway"
    echo ""
    echo "Available Commands:"
    echo "  create   Create NFS gateway"
    echo ""
    echo "Flags:"
    echo "  -h, --help   help for nfs"
    ;;
  "gateway nfs create --help")
    echo "Create NFS gateway"
    echo ""
    echo "Usage:"
    echo "  fake gateway nfs create --resource <name> --service-ip <ip/cidr>"
    echo ""
    echo "Flags:"
    echo "      --service-ip string   Service IP"
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIHelpWalksTheWholeTree(t *testing.T) {
	doc, st, err := CLIHelp(context.Background(), fakeCLI(t))
	if err != nil {
		t.Fatalf("CLIHelp: %v", err)
	}
	// root + gateway + gateway nfs + gateway nfs create
	if st.Commands != 4 {
		t.Fatalf("walked %d commands, want 4:\n%s", st.Commands, doc)
	}
	// The leaf is the whole point: it carries the flag spelling an agent would
	// otherwise invent.
	if !strings.Contains(doc, "### fake-cli gateway nfs create") {
		t.Error("the leaf subcommand's section is missing")
	}
	if !strings.Contains(doc, "--service-ip string") {
		t.Error("the leaf's flags were not captured")
	}
}

// cobra generates help and completion for every command; ingesting them adds
// one near-identical page per shell and documents the help system rather than
// the product.
func TestCLIHelpSkipsGeneratedSubcommands(t *testing.T) {
	doc, _, err := CLIHelp(context.Background(), fakeCLI(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"### fake-cli help", "### fake-cli completion"} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("%q should not be walked", unwanted)
		}
	}
}

func TestCLIHelpRejectsANonCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CLIHelp(context.Background(), path); err == nil {
		t.Fatal("a binary that produces no help must be reported, not ingested as an empty document")
	}
}

// A CLI whose help does not use a recognised "Available Commands:" block still
// yields its top-level page rather than an error.
func TestCLIHelpHandlesUnfamiliarFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'usage: odd [thing]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc, st, err := CLIHelp(context.Background(), path)
	if err != nil {
		t.Fatalf("CLIHelp: %v", err)
	}
	if st.Commands != 1 || !strings.Contains(doc, "usage: odd") {
		t.Fatalf("commands=%d doc=%q", st.Commands, doc)
	}
}
