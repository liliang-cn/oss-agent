// Package config loads runtime configuration from the environment.
// Cloud frontier models only (OpenAI-compatible API). No local models.
package config

import (
	"os"
	"strconv"
)

type Config struct {
	// LLM (reasoning / orchestration brain)
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string

	// Embedder (for cortexdb GraphRAG memory)
	EmbBaseURL string
	EmbAPIKey  string
	EmbModel   string

	// On-disk store for the agent's graph memory (cortexdb).
	DBPath string

	// Knowledge base (GraphRAG over the active domain's material).
	KnowledgeDBPath string
	EmbDim          int

	// Path to the active domain config (domain.toml). Each project supplies its own.
	DomainFile string
}

// Load reads config from environment variables with sensible defaults.
//
//	OSS_LLM_BASE_URL / OSS_LLM_API_KEY / OSS_LLM_MODEL
//	OSS_EMB_BASE_URL / OSS_EMB_API_KEY / OSS_EMB_MODEL
//	OSS_DB_PATH
func Load() Config {
	llmBase := env("OSS_LLM_BASE_URL", "https://api.openai.com/v1")
	llmKey := os.Getenv("OSS_LLM_API_KEY")
	return Config{
		LLMBaseURL: llmBase,
		LLMAPIKey:  llmKey,
		LLMModel:   env("OSS_LLM_MODEL", "gpt-4o"),
		EmbBaseURL: env("OSS_EMB_BASE_URL", llmBase),
		EmbAPIKey:  env("OSS_EMB_API_KEY", llmKey),
		EmbModel:        env("OSS_EMB_MODEL", "text-embedding-3-small"),
		DBPath:          env("OSS_DB_PATH", "./data/oss-agent.db"),
		KnowledgeDBPath: env("OSS_KNOWLEDGE_DB_PATH", "./data/knowledge.db"),
		EmbDim:          envInt("OSS_EMB_DIM", 1536),
		DomainFile:      env("OSS_DOMAIN_FILE", "./domain.toml"),
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
