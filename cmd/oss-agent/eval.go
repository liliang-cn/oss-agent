package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	evalgo "github.com/liliang-cn/eval-go"
	"github.com/liliang-cn/eval-go/llmjudge"

	"github.com/liliang-cn/agent-go/v2/pkg/llm"

	"github.com/liliang-cn/oss-agent/internal/agents"
	"github.com/liliang-cn/oss-agent/internal/cite"
	"github.com/liliang-cn/oss-agent/internal/config"
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
	topK := fs.Int("k", 6, "knowledge chunks retrieved as context per question")
	conc := fs.Int("concurrency", 2, "parallel judge evaluations")
	failUnder := fs.Float64("fail-under", 0, "exit non-zero if any metric's pass-rate is below this (0 = off)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fail("usage: oss-agent eval <dataset.json> [-out report.json] [-k 6] [-fail-under 0.8]\n" +
			"  dataset: a JSON array of {name, question, category?, meta?} (or {\"cases\":[...]})")
	}

	cases := loadEvalDataset(fs.Arg(0))
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
	for i, c := range cases {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", i+1, len(cases), c.Name)

		// Retrieve the same context the agent grounds on, for the RAG judges.
		gr, _ := store.SearchGraph(ctx, c.Question, *topK)
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
	if report.Failed() {
		os.Exit(1)
	}
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
