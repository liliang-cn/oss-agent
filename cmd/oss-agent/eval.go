package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	evalgo "github.com/liliang-cn/eval-go"
	"github.com/liliang-cn/eval-go/llmjudge"

	"github.com/liliang-cn/agent-go/v3/pkg/llm"

	"github.com/liliang-cn/oss-agent/internal/agents"
	"github.com/liliang-cn/oss-agent/internal/cite"
	"github.com/liliang-cn/oss-agent/internal/config"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
)

// evalCase is one row of an eval dataset: a question to ask the agent. Expected
// answers aren't required — the RAG metrics judge groundedness/relevancy against
// the retrieved context, not a gold answer.
type evalCase struct {
	Name     string            `json:"name"`
	Question string            `json:"question"`
	Category string            `json:"category,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// runEval runs the agent over a dataset and scores each answer for RAG quality:
// groundedness (Faithfulness), answer relevancy, retrieval quality (ContextualPrecision),
// and citation coverage — using eval-go with an LLM judge.
func runEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	out := fs.String("out", "", "also write the JSON report to this file")
	topK := fs.Int("k", 4, "knowledge chunks retrieved as context per question (matches the agent's knowledge_search)")
	conc := fs.Int("concurrency", 2, "parallel judge evaluations")
	failUnder := fs.Float64("fail-under", 0, "exit non-zero if any metric's pass-rate is below this (0 = off)")
	requireProse := fs.Bool("require-prose", false,
		"fail any case whose category is \"ops\" that retrieved no prose — guards against an imported code graph answering operational questions")
	// flag stops at the first positional, so a leading dataset path would swallow
	// any flags after it. Pull a leading non-flag arg out first so order is free.
	dsArg := ""
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dsArg, rest = args[0], args[1:]
	}
	_ = fs.Parse(rest)
	if dsArg == "" {
		dsArg = fs.Arg(0)
	}
	if dsArg == "" {
		fail("usage: oss-agent eval <dataset.json> [-k 4] [-out report.json] [-fail-under 0.8]\n" +
			"  dataset: a JSON array of {name, question, category?, meta?} (or {\"cases\":[...]})")
	}

	cases := loadEvalDataset(dsArg)
	if len(cases) == 0 {
		fail("dataset %s has no cases", fs.Arg(0))
	}

	cfg := config.Load()
	dom := loadDomain(cfg)
	svc, store, err := agents.Build(cfg, dom)
	if err != nil {
		fail("build agent: %v", err)
	}
	defer svc.Close()
	defer store.Close()

	gen, err := agents.LLM(cfg)
	if err != nil {
		fail("init judge llm: %v", err)
	}
	judge := llmjudge.New(llm.NewService(gen))

	ctx := context.Background()
	samples := make([]evalgo.Sample, 0, len(cases))
	mixes := make([]sourceMix, 0, len(cases))
	for i, c := range cases {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", i+1, len(cases), c.Name)

		// Retrieve the same context the agent grounds on, for the RAG judges.
		// Retrieval failures are non-fatal: the sample is still judged, just
		// without retrieved context (the RAG metrics then score it as ungrounded).
		gr, rerr := store.SearchGraph(ctx, c.Question, *topK)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "  retrieval failed: %v\n", rerr)
			gr = &knowledge.GraphResult{}
		}
		contexts := make([]string, 0, len(gr.Hits))
		sources := make([]string, 0, len(gr.Hits))
		seen := map[string]bool{}
		for _, h := range gr.Hits {
			contexts = append(contexts, h.Content)
			if h.DocumentID != "" && !seen[h.DocumentID] {
				seen[h.DocumentID] = true
				sources = append(sources, h.DocumentID)
			}
		}

		answer, err := svc.Ask(ctx, c.Question)
		if err != nil {
			answer = "[error] " + err.Error()
		}
		// Mirror what users see: append the deterministic Sources footer so the
		// citation-coverage metric scores the real, shipped answer shape.
		answer += cite.Footer(answer, sources)

		samples = append(samples, evalgo.Sample{
			Name:    c.Name,
			Input:   c.Question,
			Output:  answer,
			Context: contexts,
			Meta:    c.Meta,
		})
		mixes = append(mixes, sourceMixOf(c, gr.Hits))
	}

	suite := evalgo.Suite{
		Samples: samples,
		Metrics: []evalgo.Metric{
			evalgo.NonEmpty(),
			evalgo.CitationPresent(),
			evalgo.Faithfulness(judge),
			evalgo.AnswerRelevancy(judge, 0.5),
			evalgo.ContextualPrecision(judge, 0.5),
		},
		Concurrency: *conc,
	}
	report := suite.Run(ctx)
	report.WriteConsole(os.Stdout)
	fmt.Fprint(os.Stdout, formatSourceMix(mixes))

	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fail("write report: %v", err)
		}
		_ = report.WriteJSON(f)
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	}

	if *failUnder > 0 {
		for _, ms := range report.Summary() {
			if ms.PassRate < *failUnder {
				fail("metric %s pass-rate %.2f below threshold %.2f", ms.Metric, ms.PassRate, *failUnder)
			}
		}
	}
	if *requireProse {
		var starved []string
		for _, m := range mixes {
			if m.Category == "ops" && m.Prose == 0 {
				starved = append(starved, m.Name)
			}
		}
		if len(starved) > 0 {
			fail("%d ops question(s) retrieved no prose at all: %s\n"+
				"  An imported code graph can outnumber the runbooks and take every slot; the answer\n"+
				"  stays fluent while its sources change, which no answer-quality metric catches.",
				len(starved), strings.Join(starved, ", "))
		}
	}
	if report.Failed() {
		os.Exit(1)
	}
}

// sourceMix is where one question's retrieved context came from.
//
// The answer-quality metrics above cannot see this: when an imported code graph
// crowded the runbooks out of a live store, the answers stayed fluent and
// well-formed — an operational question was simply answered from the struct that
// models the thing rather than from the runbook that says what to do about it.
// Only the sources changed, so only the sources reveal it.
type sourceMix struct {
	Name     string
	Category string
	Code     int
	Prose    int
}

func sourceMixOf(c evalCase, hits []knowledge.Hit) sourceMix {
	m := sourceMix{Name: c.Name, Category: c.Category}
	for _, h := range hits {
		if knowledge.IsCodeDocumentID(h.DocumentID) {
			m.Code++
			continue
		}
		m.Prose++
	}
	return m
}

func formatSourceMix(mixes []sourceMix) string {
	if len(mixes) == 0 {
		return ""
	}
	var b strings.Builder
	var code, prose, starved int
	b.WriteString("\nRetrieved sources (code = imported code graph, prose = docs and runbooks)\n")
	for _, m := range mixes {
		code += m.Code
		prose += m.Prose
		flag := ""
		if m.Category == "ops" && m.Prose == 0 {
			flag = "  <- ops question, no prose"
			starved++
		}
		cat := m.Category
		if cat == "" {
			cat = "-"
		}
		fmt.Fprintf(&b, "  %-28s %-6s code=%-3d prose=%-3d%s\n", truncate(m.Name, 28), cat, m.Code, m.Prose, flag)
	}
	fmt.Fprintf(&b, "  total: code=%d prose=%d\n", code, prose)
	if starved > 0 {
		fmt.Fprintf(&b, "  %d ops question(s) retrieved no prose — re-run with -require-prose to fail on this\n", starved)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func loadEvalDataset(path string) []evalCase {
	raw, err := os.ReadFile(path)
	if err != nil {
		fail("read dataset: %v", err)
	}
	var cases []evalCase
	if json.Unmarshal(raw, &cases) == nil && len(cases) > 0 {
		return cases
	}
	var wrap struct {
		Cases []evalCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		fail("parse dataset (want a JSON array of cases or {\"cases\":[...]}): %v", err)
	}
	return wrap.Cases
}
