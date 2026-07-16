// providers.go — the in-process BYOK LLM layer.
//
// The vendored CLI is purely heuristic/keyless: it never reads a provider key
// and never calls an LLM. All LLM work therefore happens HERE, in the web
// layer, as a post-processing step: the CLI's JSON output plus the user's
// claim(s) are sent in ONE chat-completions call to the caller-selected
// provider, which returns a structured synthesis (stance, confidence,
// reasoning, key evidence points, and now an explicit list of studies it
// judged off-topic or methodologically too weak to count). The CLI result is
// always returned verbatim; an LLM failure degrades to the heuristic result
// plus a redacted llm_error.
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

// excludedStudy is one study the LLM judged should not count toward the
// verdict, with a short human-readable reason (off-topic, wrong subject,
// animal/in-vitro only, etc.). Title mirrors the CLI study title so the UI can
// match it against the displayed cards.
type excludedStudy struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// llmSynthesis is the structured post-processing verdict returned to clients
// under "llm_synthesis". KeyEvidence holds 3-5 points referencing the CLI data.
// ExcludedStudies lists the studies the model set aside as irrelevant or too
// methodologically weak, so the filtering is transparent rather than silent.
type llmSynthesis struct {
	Stance          string          `json:"stance"` // supports | refutes | mixed | insufficient
	Confidence      float64         `json:"confidence"`
	Reasoning       string          `json:"reasoning"`
	KeyEvidence     []string        `json:"key_evidence"`
	ExcludedStudies []excludedStudy `json:"excluded_studies"`
	Model           string          `json:"model"`
}

// maxCLIJSONForPrompt caps how much CLI output is embedded in the prompt so
// large study lists cannot blow the model's context window. Raised from 24KB
// to 56KB so the model sees more of the study list and can judge relevance on
// more titles; still well inside typical context limits.
const maxCLIJSONForPrompt = 56 * 1024

// maxStudiesForLLM caps how many all_studies entries the LLM sees — bump this
// one line to widen the LLM's view. The list is relevance-ordered, so the trim
// keeps the most relevant studies; with ≤1500-char abstracts, 25 entries stay
// comfortably inside maxCLIJSONForPrompt. Without the trim, the naive byte cap
// cut the all_studies array mid-JSON (it is the LAST field of the CLI output),
// silently dropping exactly the data the LLM needs most while keeping the
// duplicated top_supporting/top_refuting abstracts.
const maxStudiesForLLM = 25

// maxStudiesForCompare is the per-claim cap for compare output: compare packs
// 2 claims into one prompt, so use a smaller per-claim cap to stay under the
// byte cap (2 × ~12 studies with ≤1500-char abstracts ≈ ~45KB, inside the 56KB
// maxCLIJSONForPrompt; 2 × 25 would exceed it and get cut mid-JSON).
const maxStudiesForCompare = 12

// compactForLLM shrinks the CLI JSON before it is embedded in the prompt.
// Wherever an object carries an "all_studies" array (consensus output, and the
// claim_a/claim_b sub-objects of compare output), the array is trimmed and the
// top_supporting/top_refuting lists are dropped — all_studies supersedes them,
// and keeping both would send each top study's abstract twice. The trim cap
// depends on the shape: compare output (a top-level claim_a/claim_b key) uses
// the smaller maxStudiesForCompare per claim so both study lists fit under
// maxCLIJSONForPrompt together; everything else uses maxStudiesForLLM. Output
// without all_studies (evidence, gaps, controversies, or an older CLI binary)
// is returned unchanged, as is anything that fails to parse. Only the LLM's
// copy is compacted; the client always receives the CLI JSON verbatim.
func compactForLLM(raw []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	maxStudies := maxStudiesForLLM
	if _, isCompare := obj["claim_a"]; isCompare {
		maxStudies = maxStudiesForCompare
	} else if _, isCompare := obj["claim_b"]; isCompare {
		maxStudies = maxStudiesForCompare
	}
	if !compactStudyObject(obj, maxStudies) {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// compactStudyObject applies the all_studies trim (to maxStudies entries) +
// top-list removal to one object and recurses into compare's claim_a/claim_b
// sub-objects with the same cap. Returns true when anything changed.
func compactStudyObject(obj map[string]json.RawMessage, maxStudies int) bool {
	changed := false
	if rawList, ok := obj["all_studies"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(rawList, &list); err == nil {
			if len(list) > maxStudies {
				if trimmed, err := json.Marshal(list[:maxStudies]); err == nil {
					obj["all_studies"] = trimmed
				}
			}
			delete(obj, "top_supporting")
			delete(obj, "top_refuting")
			changed = true
		}
	}
	for _, k := range []string{"claim_a", "claim_b"} {
		sub, ok := obj[k]
		if !ok {
			continue
		}
		var subObj map[string]json.RawMessage
		if err := json.Unmarshal(sub, &subObj); err != nil {
			continue
		}
		if compactStudyObject(subObj, maxStudies) {
			if enc, err := json.Marshal(subObj); err == nil {
				obj[k] = enc
				changed = true
			}
		}
	}
	return changed
}

// synthesisPrompt builds the single user message sent to the LLM. It now asks
// the model to act as a strict relevance/quality filter: examine each study by
// abstract content (title as fallback), set aside off-topic or methodologically
// weak entries, base the verdict only on what genuinely bears on the claim, and
// report what it excluded. The CLI JSON is compacted first (compactForLLM) so
// the LLM works from the relevance-ordered all_studies list when present.
func synthesisPrompt(endpoint string, claims []string, cliJSON []byte) string {
	cliJSON = compactForLLM(cliJSON)
	truncated := false
	if len(cliJSON) > maxCLIJSONForPrompt {
		cliJSON = cliJSON[:maxCLIJSONForPrompt]
		truncated = true
	}
	var b strings.Builder
	b.WriteString("You are a rigorous evidence-synthesis assistant. Below is the JSON output of a scientific-literature analysis tool (command: " + endpoint + ") for the claim(s):\n")
	for i, c := range claims {
		fmt.Fprintf(&b, "CLAIM %d: %s\n", i+1, c)
	}
	b.WriteString("\nTOOL OUTPUT (may be truncated):\n")
	b.Write(cliJSON)
	if truncated {
		b.WriteString("\n[NOTE: tool output was truncated; some studies may not be shown.]")
	}
	b.WriteString("\n\nIMPORTANT — the tool matched studies by keyword and did NOT verify that each study is actually about the claim. When an \"all_studies\" array is present, it is the complete analyzed study list: ordered by search relevance (NOT by citation count), capped to the most relevant entries, each with an \"abstract\" field when the source provides one. Before forming a verdict, act as a strict filter:\n" +
		"1. Examine each study by its abstract when present — judge relevance and study design from the abstract's content, not from the title alone (title only when the abstract is empty).\n" +
		"2. EXCLUDE a study when it is clearly off-topic (not about the claim's specific subject and outcome), when it studies a different substance, or when its design cannot support the claim about humans (e.g. animal-only or in-vitro/cell-culture studies used to assert a human body-weight or clinical effect).\n" +
		"3. Base your stance, confidence, and key_evidence ONLY on the studies that remain after exclusion. Prefer higher-tier human evidence (meta-analyses, systematic reviews, RCTs) over observational or mechanistic studies, and note reverse-causality limits for observational data where relevant.\n" +
		"4. If too few genuinely relevant studies remain, say so and use stance \"insufficient\".\n\n" +
		"Respond with ONLY a JSON object, no markdown fences, with exactly these fields:\n" +
		`{"stance":"supports|refutes|mixed|insufficient","confidence":0.0,"reasoning":"2-4 sentence synthesis based only on the studies you kept","key_evidence":["3-5 short points, each referencing specific numbers or studies you kept"],"excluded_studies":[{"title":"study title copied from the tool output","reason":"short reason, e.g. off-topic: about X not the claim / animal model only / in-vitro only / different substance"}]}` + "\n" +
		"stance is your overall verdict on claim 1 (for comparisons, weigh both claims and explain in reasoning). confidence is 0-1. Leave excluded_studies as [] only if every study is genuinely relevant.")
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
	// Normalize excluded studies: drop entries with an empty title, trim fields,
	// and cap the list so a runaway model response can't bloat the payload.
	cleaned := make([]excludedStudy, 0, len(syn.ExcludedStudies))
	for _, e := range syn.ExcludedStudies {
		t := strings.TrimSpace(e.Title)
		if t == "" {
			continue
		}
		r := strings.TrimSpace(e.Reason)
		if len(t) > 300 {
			t = t[:300]
		}
		if len(r) > 200 {
			r = r[:200]
		}
		cleaned = append(cleaned, excludedStudy{Title: t, Reason: r})
		if len(cleaned) >= 20 {
			break
		}
	}
	syn.ExcludedStudies = cleaned
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