// Package probes runs READ-ONLY diagnostic commands the agent may call during a
// ReAct loop. The command set is product-specific and supplied by the active
// Domain (config), not hardcoded here. Every probe is passed through the red-line
// safety filter as defense-in-depth, so a probe can never become a mutation.
package probes

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/liliang-cn/oss-agent/internal/safety"
)

// Probe is one read-only diagnostic command with a fixed argv. It is loaded from
// the domain config, hence the toml tags.
type Probe struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Argv        []string `toml:"argv"`
}

// Run executes a probe's argv with a timeout and returns combined stdout+stderr.
// stderr is intentionally returned even on non-zero exit — it is the diagnostic
// signal. If a filter is provided, a command tripping a red-line is refused
// (defense-in-depth: probes are read-only, but never let one become a mutation).
func Run(ctx context.Context, f *safety.Filter, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	if f != nil {
		if v := f.Check(strings.Join(argv, " ")); v.Blocked {
			return "", fmt.Errorf("refused by red-line wall [%s]: %s", v.RuleID, v.Reason)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}
