// Command lib-example shows how to embed oss-agent as a library.
//
// It imports only the public facade (github.com/liliang-cn/oss-agent) — never an
// internal package — so it mirrors how an external project would use it.
//
//	OSS_LLM_API_KEY=... OSS_DOMAIN_FILE=domain.toml OSS_KNOWLEDGE_DB_PATH=knowledge.db \
//	  go run ./examples/lib "how do I recover a degraded resource?"
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	ossagent "github.com/liliang-cn/oss-agent"
)

func main() {
	question := strings.Join(os.Args[1:], " ")
	if question == "" {
		question = "How do I safely recover a resource stuck in a degraded state?"
	}

	// Zero-value fields fall back to OSS_* env vars, then defaults.
	a, err := ossagent.New(ossagent.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	defer a.Close()

	fmt.Printf("domain: %s\n\n", a.Domain().Name)

	// 1) One-shot retrieval (no LLM) — inspect what the agent would ground on.
	if hits, err := a.Search(context.Background(), question, 4); err == nil {
		fmt.Println("top sources:")
		for _, h := range hits {
			fmt.Printf("  - %s\n", h.DocumentID)
		}
		fmt.Println()
	}

	// 2) The deterministic safety wall is usable on its own.
	if v := a.CheckCommand("wipefs -a /dev/sdb"); v.Blocked {
		fmt.Printf("safety: blocked %q (%s)\n\n", "wipefs -a /dev/sdb", v.Reason)
	}

	// 3) Stream a grounded answer, printing tool calls live.
	fmt.Println("--- answer ---")
	_, sources, err := a.Stream(context.Background(), question, func(e ossagent.Event) {
		switch e.Kind {
		case ossagent.EventToolCall:
			fmt.Printf("\n[tool %s %v]\n", e.Tool, e.Args)
		case ossagent.EventReset:
			fmt.Print("\n\n(replacing preamble with final answer)\n\n")
		case ossagent.EventText:
			fmt.Print(e.Text)
		case ossagent.EventError:
			fmt.Fprintln(os.Stderr, "\n[error]", e.Text)
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nstream:", err)
		os.Exit(1)
	}
	fmt.Printf("\n\n(%d sources cited)\n", len(sources))
}
