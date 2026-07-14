// providers.go — the in-process BYOK LLM layer.
//
// The vendored CLI is purely heuristic/keyless: it never reads a provider key
// and never calls an LLM. All LLM work therefore happens HERE, in the web
// layer, as a post-processing step: the CLI's JSON output plus the user's
// claim(s) are sent in ONE chat-completions call to the caller-selected
// provider, which returns a structured synthesis (stance, confidence,
// reasoning, key evidence points). The CLI result is always returned verbatim;
// an LLM failure degrades to the heuristic result plus a redacted llm_error.
//
// SECURITY MODEL (same rules as main.go, do not weaken):
//   - The key lives in memory for one request and goes into exactly one
//     outbound Authorization/x-api-key header over HTTPS. Never logged,
//     never persisted, never placed in any environment.
//   - Every error string that could contain a provider response body passes
//     through redact() (exact-key removal) plus control-byte stripping and
//     truncation before it reaches a client or a log line.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// llmTimeout bounds the single synthesis call; it must fit inside the 120s
// request budget in runCLIJSON with room for the CLI run that precedes it.
const llmTimeout = 60 * time.Second

// authStyle selects how the key is presented and which wire format is used.
type authStyle int

const (
	styleOpenAI    authStyle = iota // POST {base}/chat/completions, Authorization: Bearer
	styleAnthropic                  // POST {base}/messages, x-api-key + anthropic-version
)

// providerSpec describes one BYOK provider. BaseURL is the API root WITHOUT
// the /chat/completions (or /messages) suffix; DefaultModel is used when the
// caller sends no model override.
type providerSpec struct {
	BaseURL      string
	DefaultModel string
	Style        authStyle
}

// providers is the full BYOK registry. Everything except anthropic speaks the
// OpenAI chat-completions format; gemini via Google's OpenAI-compatibility
// endpoint, qwen via DashScope's international compatible-mode endpoint.
// openrouter is a meta-provider: its model string selects any hosted model
// (including :free ones), so the UI treats model as effectively required there.
var providers = map[string]providerSpec{
	"anthropic":  {"https://api.anthropic.com/v1", "claude-haiku-4-5", styleAnthropic},
	"openai":     {"https://api.openai.com/v1", "gpt-5-mini", styleOpenAI},
	"gemini":     {"https://generativelanguage.googleapis.com/v1beta/openai", "gemini-2.5-flash", styleOpenAI},
	"groq":       {"https://api.groq.com/openai/v1", "llama-3.3-70b-versatile", styleOpenAI},
	"mistral":    {"https://api.mistral.ai/v1", "mistral-small-latest", styleOpenAI},
	"deepseek":   {"https://api.deepseek.com", "deepseek-chat", styleOpenAI},
	"zai":        {"https://api.z.ai/api/paas/v4", "glm-5", styleOpenAI},
	"moonshot":   {"https://api.moonshot.ai/v1", "kimi-k2.6", styleOpenAI},
	"qwen":       {"https://dashscope-intl.aliyuncs.com/compatible-mode/v1", "qwen3-max", styleOpenAI},
	"minimax":    {"https://api.minimax.io/v1", "MiniMax-M2.7", styleOpenAI},
	"xai":        {"https://api.x.ai/v1", "grok-4-fast", styleOpenAI},
	"openrouter": {"https://openrouter.ai/api/v1", "deepseek/deepseek-chat", styleOpenAI},
}

// supportedProviders is the sorted name list used in error messages.
var supportedProviders = func() string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}()

// llmSynthesis is the structured post-processing verdict returned to clients
// under "llm_synthesis". KeyEvidence holds 3-5 points referencing the CLI data.
type llmSynthesis struct {
	Stance      string   `json:"stance"` // supports | refutes | mixed | insufficient
	Confidence  float64  `json:"confidence"`
	Reasoning   string   `json:"reasoning"`
	KeyEvidence []string `json:"key_evidence"`
	Model       string   `json:"model"`
}

// maxCLIJSONForPrompt caps how much CLI output is embedded in the prompt so
// large study lists cannot blow the model's context window.
const maxCLIJSONForPrompt = 24 * 1024

// synthesisPrompt builds the single user message sent to the LLM.
func synthesisPrompt(endpoint string, claims []string, cliJSON []byte) string {
	if len(cliJSON) > maxCLIJSONForPrompt {
		cliJSON = cliJSON[:maxCLIJSONForPrompt]
	}
	var b strings.Builder
	b.WriteString("You are an evidence-synthesis assistant. Below is the JSON output of a scientific-literature analysis tool (command: " + endpoint + ") for the claim(s):\n")
	for i, c := range claims {
		fmt.Fprintf(&b, "CLAIM %d: %s\n", i+1, c)
	}
	b.WriteString("\nTOOL OUTPUT (may be truncated):\n")
	b.Write(cliJSON)
	b.WriteString("\n\nRespond with ONLY a JSON object, no markdown fences, with exactly these fields:\n" +
		`{"stance":"supports|refutes|mixed|insufficient","confidence":0.0,"reasoning":"2-4 sentence synthesis","key_evidence":["3-5 short points, each referencing specific numbers or studies from the tool output"]}` + "\n" +
		"stance is your overall verdict on claim 1 (for comparisons, weigh both claims and explain in reasoning). confidence is 0-1.")
	return b.String()
}

// openAIRequest / anthropicRequest are the minimal wire shapes. temperature is
// deliberately omitted (some providers reject non-default values).
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type anthropicRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []chatMessage `json:"messages"`
}

// llmSynthesize makes one chat call to the selected provider and parses the
// structured synthesis. Every returned error is already safe to expose: key
// redacted, control bytes stripped, body truncated.
func llmSynthesize(ctx context.Context, provider, key, model, endpoint string, claims []string, cliJSON []byte) (*llmSynthesis, error) {
	spec, ok := providers[provider]
	if !ok { // callers validate first; belt and braces
		return nil, errors.New("unknown provider")
	}
	if model == "" {
		model = spec.DefaultModel
	}
	prompt := synthesisPrompt(endpoint, claims, cliJSON)

	var url string
	var payload any
	switch spec.Style {
	case styleAnthropic:
		url = spec.BaseURL + "/messages"
		payload = anthropicRequest{Model: model, MaxTokens: 1024, Messages: []chatMessage{{Role: "user", Content: prompt}}}
	default:
		url = spec.BaseURL + "/chat/completions"
		payload = openAIRequest{Model: model, Messages: []chatMessage{{Role: "user", Content: prompt}}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build request: " + sanitizeLLMError(err.Error(), key))
	}
	req.Header.Set("Content-Type", "application/json")
	if spec.Style == styleAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Transport errors can embed the URL but never the key (it travels in a
		// header); sanitize anyway.
		return nil, errors.New("request failed: " + sanitizeLLMError(err.Error(), key))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("read response: " + sanitizeLLMError(err.Error(), key))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, sanitizeLLMError(string(respBody), key))
	}

	text, err := extractChatText(spec.Style, respBody)
	if err != nil {
		return nil, errors.New(sanitizeLLMError(err.Error(), key))
	}
	syn, err := parseSynthesis(text)
	if err != nil {
		return nil, errors.New("unparseable synthesis: " + sanitizeLLMError(err.Error(), key))
	}
	syn.Model = model
	return syn, nil
}

// extractChatText pulls the assistant text out of the provider response.
func extractChatText(style authStyle, body []byte) (string, error) {
	if style == styleAnthropic {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return "", errors.New("invalid provider response JSON")
		}
		for _, c := range r.Content {
			if c.Type == "text" && c.Text != "" {
				return c.Text, nil
			}
		}
		return "", errors.New("provider response contained no text content")
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", errors.New("invalid provider response JSON")
	}
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == "" {
		return "", errors.New("provider response contained no choices")
	}
	return r.Choices[0].Message.Content, nil
}

// parseSynthesis parses the model's JSON verdict, tolerating markdown fences
// and surrounding prose, then normalizes the fields.
func parseSynthesis(text string) (*llmSynthesis, error) {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return nil, errors.New("no JSON object in model output")
	}
	var syn llmSynthesis
	if err := json.Unmarshal([]byte(text[start:end+1]), &syn); err != nil {
		return nil, errors.New("model output is not valid JSON")
	}
	switch syn.Stance {
	case "supports", "refutes", "mixed", "insufficient":
	default:
		syn.Stance = "insufficient"
	}
	if syn.Confidence < 0 {
		syn.Confidence = 0
	} else if syn.Confidence > 1 {
		syn.Confidence = 1
	}
	if len(syn.KeyEvidence) > 5 {
		syn.KeyEvidence = syn.KeyEvidence[:5]
	}
	return &syn, nil
}

// sanitizeLLMError makes an upstream diagnostic safe for clients and logs:
// exact key redaction, control bytes stripped, hard length cap.
func sanitizeLLMError(s, key string) string {
	s = redact(s, key)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' {
			return ' '
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return strings.TrimSpace(s)
}

// validateModel enforces the opaque-token rules for the caller-supplied model
// override: trimmed, ≤128 chars, no whitespace or control characters. Returns
// the normalized model and "" on success, or an error message (which never
// echoes the value — the model field is treated as sensitive-adjacent input).
func validateModel(model string) (string, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", ""
	}
	if len(model) > 128 {
		return "", "model must be at most 128 characters"
	}
	for _, r := range model {
		if r <= 0x20 || r == 0x7f {
			return "", "model must not contain whitespace or control characters"
		}
	}
	return model, ""
}
