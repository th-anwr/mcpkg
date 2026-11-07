// Package llm is a tiny client for any OpenAI-compatible chat completions API:
// vLLM / Ollama / llama.cpp server (self-hosted open models), DeepInfra, Together,
// Groq, OpenRouter, or Amazon Bedrock's OpenAI-compatible endpoint.
package llm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
)

// Client calls POST {BaseURL}/chat/completions.
type Client struct {
	BaseURL  string
	APIKey   string
	Model    string
	JSONMode bool
	HTTP     *httpx.Client
	Log      *slog.Logger // optional: Debug per call, Warn on failure
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage reports token counts for cost tracking.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Enabled reports whether an endpoint is configured.
func (c *Client) Enabled() bool { return c != nil && c.BaseURL != "" && c.Model != "" }

// Chat returns the assistant text.
func (c *Client) Chat(ctx context.Context, msgs []Message, maxTokens int) (string, Usage, error) {
	if !c.Enabled() {
		return "", Usage{}, errors.New("llm not configured")
	}
	body := map[string]any{
		"model":       c.Model,
		"messages":    msgs,
		"temperature": 0.1,
		"max_tokens":  maxTokens,
	}
	if c.JSONMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	hdr := map[string]string{}
	if c.APIKey != "" {
		hdr["Authorization"] = "Bearer " + c.APIKey
	}
	var res struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	start := time.Now()
	if err := c.HTTP.PostJSON(ctx, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", hdr, body, &res); err != nil {
		if c.Log != nil {
			c.Log.Warn("llm call failed", "model", c.Model, "ms", time.Since(start).Milliseconds(), "err", err)
		}
		return "", Usage{}, err
	}
	if len(res.Choices) == 0 {
		return "", res.Usage, errors.New("llm returned no choices")
	}
	if c.Log != nil {
		c.Log.Debug("llm call", "model", c.Model, "ms", time.Since(start).Milliseconds(),
			"prompt_tokens", res.Usage.PromptTokens, "completion_tokens", res.Usage.CompletionTokens,
			"finish_reason", res.Choices[0].FinishReason)
	}
	return res.Choices[0].Message.Content, res.Usage, nil
}
