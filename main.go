// Command scientific-consensus-web is a thin standalone HTTP wrapper around the
// scientific-consensus CLI. Each /api endpoint runs the compiled CLI once per
// request (always keyless/heuristic — the CLI never uses an LLM), then, when the
// caller supplied a BYOK (bring-your-own-key) key, makes ONE in-process
// chat-completions call (providers.go) to synthesize the CLI output into a
// structured verdict. Each caller uses their own LLM key; the server never
// holds a key of its own.
//
// SECURITY MODEL (enforced below and in providers.go, do not weaken):
//   - The BYOK key arrives in the X-LLM-Key request header and lives in memory
//     only for the duration of one request and one outbound HTTPS call.
//   - The key is NEVER logged, printed, persisted, written to the server's own
//     process environment, or passed to the child CLI. buildChildEnv() strips
//     every known provider key out of os.Environ(), so a key set in the
//     server's own environment can never leak into the child either.
//   - Any CLI stderr or LLM provider diagnostic surfaced to the client passes
//     through redact()/sanitizeLLMError(), which remove the key substring so a
//     key echoed in an error can never escape.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cliBinary is the compiled scientific-consensus CLI this server shells out to.
// Overridable with CLI_BIN. Otherwise it defaults to bin/scientific-consensus-pp-cli
// (plus a .exe suffix on Windows), so the same code runs against a Windows-built
// binary locally and a Linux-built binary inside the Docker/Render container.
func cliBinaryPath() string {
	if p := strings.TrimSpace(os.Getenv("CLI_BIN")); p != "" {
		return p
	}
	name := "scientific-consensus-pp-cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join("bin", name)
}

// allProviderEnvVars is every provider key env var a CLI might conceivably
// read. buildChildEnv strips ALL of them from the inherited environment so the
// child never sees any provider key — the child is always keyless; LLM calls
// happen in-process (providers.go).
var allProviderEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GROQ_API_KEY",
	"MISTRAL_API_KEY",
}

// consensusResponse wraps the CLI's raw JSON verbatim. stance_source is
// "llm:<provider>" when an LLM synthesis succeeded, otherwise "heuristic"
// (no key supplied, or the LLM call failed — then llm_error says why, already
// redacted). llm_synthesis carries the structured verdict on success. With no
// key the response is byte-identical in shape to the pre-LLM version:
// {"stance_source":"heuristic","result":...}.
type consensusResponse struct {
	StanceSource string          `json:"stance_source"`
	LLMSynthesis *llmSynthesis   `json:"llm_synthesis,omitempty"`
	LLMError     string          `json:"llm_error,omitempty"`
	Result       json.RawMessage `json:"result"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/api/consensus", handleConsensus)
	mux.HandleFunc("/api/evidence", handleEvidence)
	mux.HandleFunc("/api/compare", handleCompare)
	mux.HandleFunc("/api/gaps", handleGaps)
	mux.HandleFunc("/api/controversies", handleControversies)

	// Address resolution, in priority order:
	//   1. $ADDR  — explicit override (host:port), used locally.
	//   2. $PORT  — Render/Heroku convention; bind 0.0.0.0 so the platform can
	//      route external traffic to the container.
	//   3. default 127.0.0.1:8090 for local development.
	addr := "127.0.0.1:8090"
	if a := strings.TrimSpace(os.Getenv("ADDR")); a != "" {
		addr = a
	} else if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		addr = "0.0.0.0:" + p
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("scientific-consensus-web listening on %s (CLI: %s)", addr, cliBinaryPath())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

// setCORS adds the CORS headers every /api response needs. Browsers refuse
// fetch() responses without these ("Failed to fetch") even same-origin in some
// embed/proxy setups, and error responses need them too or the browser hides
// the JSON error body.
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-LLM-Key, X-LLM-Provider")
}

// preflight handles the CORS preflight OPTIONS request. Returns true when the
// request was a preflight and has been fully answered.
func preflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	w.WriteHeader(http.StatusOK)
	return true
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html":
		// Serve the single-page frontend. /api/* responses carry explicit CORS
		// headers (see setCORS) so browser fetch works regardless of origin.
		// Falls back to "ok" (a plain health check) when index.html isn't
		// present next to the binary.
		if data, err := os.ReadFile("index.html"); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	default:
		http.NotFound(w, r)
	}
}

// byok holds the per-request BYOK decision: the validated provider name, the
// key, and the (validated, possibly empty) model override. A zero byok means
// keyless/heuristic.
type byok struct {
	provider string
	key      string
	model    string
}

// extractBYOK reads the X-LLM-Key header, resolves the provider (from the
// bodyProvider argument, falling back to the X-LLM-Provider header), and
// validates the optional model override. It returns the byok decision and true
// on success; on a client error it writes the response and returns false so
// the caller stops. When no key is supplied it succeeds with a heuristic
// (keyless) decision.
func extractBYOK(w http.ResponseWriter, r *http.Request, bodyProvider, bodyModel string) (byok, bool) {
	// The model override is validated even on keyless requests so a malformed
	// value fails the same way regardless of key presence. Its value is never
	// echoed back or logged.
	model, errMsg := validateModel(bodyModel)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return byok{}, false
	}
	// Key from header only (never from body — a key in a JSON body is easier to
	// accidentally log).
	key := strings.TrimSpace(r.Header.Get("X-LLM-Key"))
	if key == "" {
		return byok{}, true
	}
	provider := strings.ToLower(strings.TrimSpace(bodyProvider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(r.Header.Get("X-LLM-Provider")))
	}
	if provider == "" {
		writeError(w, http.StatusBadRequest, "X-LLM-Key supplied but no provider; set \"provider\" in body or X-LLM-Provider header")
		return byok{}, false
	}
	if _, ok := providers[provider]; !ok {
		writeError(w, http.StatusBadRequest, "unknown provider "+quoteToken(provider)+"; supported: "+supportedProviders)
		return byok{}, false
	}
	return byok{provider: provider, key: key, model: model}, true
}

// clampLimit normalizes a caller-supplied --limit into the CLI's accepted range,
// defaulting to def when out of bounds.
func clampLimit(limit, def int) int {
	if limit <= 0 || limit > 200 {
		return def
	}
	return limit
}

// cliPacingArgs is appended to every CLI invocation. OpenAlex 429s shared
// anonymous traffic aggressively; --rate-limit 0.15 (~9 req/min) keeps the CLI
// under that limit instead of tripping its adaptive backoff (which waits 60s
// per retry and blows the request budget). Pacing makes multi-request runs
// exceed the CLI's 60s default internal timeout, so --timeout is raised to
// 100s — still inside this wrapper's 120s request budget.
var cliPacingArgs = []string{"--rate-limit", "0.15", "--timeout", "100s"}

// runCLIJSON runs the CLI with the given argv (subcommand + positional args +
// flags already assembled by the caller) in an always-keyless child, then —
// when a BYOK key was supplied — performs the in-process LLM synthesis over
// the CLI's JSON output and merges it into the response. An LLM failure never
// fails the request: the heuristic result is returned with a redacted
// llm_error. It centralizes the exec, timeouts, key-redaction, and
// JSON-validation shared by every endpoint.
func runCLIJSON(w http.ResponseWriter, r *http.Request, b byok, endpoint string, claims []string, args []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	args = append(args, cliPacingArgs...)

	// #nosec G204 -- args are fixed subcommands/flags plus user text as discrete
	// argv elements (no shell); the child env carries no keys at all.
	cmd := exec.CommandContext(ctx, cliBinaryPath(), args...)
	cmd.Env = buildChildEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Redact the key from any diagnostic before it leaves the process.
		msg := redact(strings.TrimSpace(stderr.String()), b.key)
		if msg == "" {
			msg = redact(err.Error(), b.key)
		}
		writeError(w, http.StatusBadGateway, "CLI failed: "+msg)
		return
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(raw) {
		writeError(w, http.StatusBadGateway, "CLI returned non-JSON output")
		return
	}

	resp := consensusResponse{
		StanceSource: "heuristic",
		Result:       json.RawMessage(raw),
	}
	if b.key != "" {
		syn, err := llmSynthesize(ctx, b.provider, b.key, b.model, endpoint, claims, raw)
		if err != nil {
			// Already sanitized/redacted by providers.go; safe for client + log-free.
			resp.LLMError = err.Error()
		} else {
			resp.LLMSynthesis = syn
			resp.StanceSource = "llm:" + b.provider
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// decodePOST enforces POST + decodes a JSON body into dst. On any failure it
// writes the response and returns false.
func decodePOST(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// ---- single-claim endpoints (consensus, evidence, gaps, controversies) ------

// claimRequest is the shared body for the single-claim subcommands. Provider
// and Model select the in-process LLM synthesis; Model is an opaque token
// validated by validateModel and defaults to the provider's DefaultModel.
type claimRequest struct {
	Claim    string `json:"claim"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Limit    int    `json:"limit"`
}

// handleClaimCmd is the shared handler for every subcommand whose CLI shape is
// "<subcommand> <claim> --json --limit <n>". defLimit is that subcommand's CLI
// default, used when the caller omits or over-ranges limit.
func handleClaimCmd(w http.ResponseWriter, r *http.Request, subcommand string, defLimit int) {
	setCORS(w)
	if preflight(w, r) {
		return
	}
	var req claimRequest
	if !decodePOST(w, r, &req) {
		return
	}
	req.Claim = strings.TrimSpace(req.Claim)
	if req.Claim == "" {
		writeError(w, http.StatusBadRequest, "claim is required")
		return
	}
	b, ok := extractBYOK(w, r, req.Provider, req.Model)
	if !ok {
		return
	}
	args := []string{subcommand, req.Claim, "--json", "--limit", fmt.Sprintf("%d", clampLimit(req.Limit, defLimit))}
	runCLIJSON(w, r, b, subcommand, []string{req.Claim}, args)
}

func handleConsensus(w http.ResponseWriter, r *http.Request) {
	handleClaimCmd(w, r, "consensus", 40)
}

func handleEvidence(w http.ResponseWriter, r *http.Request) {
	handleClaimCmd(w, r, "evidence", 50)
}

func handleGaps(w http.ResponseWriter, r *http.Request) {
	handleClaimCmd(w, r, "gaps", 60)
}

func handleControversies(w http.ResponseWriter, r *http.Request) {
	handleClaimCmd(w, r, "controversies", 50)
}

// ---- two-claim endpoint (compare) -------------------------------------------

type compareRequest struct {
	Claim1   string `json:"claim1"`
	Claim2   string `json:"claim2"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Limit    int    `json:"limit"`
}

func handleCompare(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if preflight(w, r) {
		return
	}
	var req compareRequest
	if !decodePOST(w, r, &req) {
		return
	}
	req.Claim1 = strings.TrimSpace(req.Claim1)
	req.Claim2 = strings.TrimSpace(req.Claim2)
	if req.Claim1 == "" || req.Claim2 == "" {
		writeError(w, http.StatusBadRequest, "both claim1 and claim2 are required")
		return
	}
	b, ok := extractBYOK(w, r, req.Provider, req.Model)
	if !ok {
		return
	}
	args := []string{"compare", req.Claim1, req.Claim2, "--json", "--limit", fmt.Sprintf("%d", clampLimit(req.Limit, 40))}
	runCLIJSON(w, r, b, "compare", []string{req.Claim1, req.Claim2}, args)
}

// buildChildEnv returns the environment for the child CLI process: the
// server's own environment with EVERY provider key removed. The child is
// always keyless — BYOK keys are used only for the in-process LLM call and
// must never reach a subprocess.
func buildChildEnv() []string {
	strip := make(map[string]struct{}, len(allProviderEnvVars))
	for _, v := range allProviderEnvVars {
		strip[v] = struct{}{}
	}
	base := os.Environ()
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if _, drop := strip[name]; drop {
			continue // never inherit the server's own provider keys
		}
		out = append(out, kv)
	}
	return out
}

// redact removes the raw key substring from s so a key that appears in CLI
// stderr can never be returned to a client or written to a log. It is a
// belt-and-braces measure on top of the CLI's own credential redaction.
func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[REDACTED]")
}

// quoteToken quotes a short untrusted token for safe inclusion in an error
// message, stripping control bytes so it can't echo terminal escapes.
func quoteToken(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return "\"" + strings.Map(func(r rune) rune {
		if r < 0x20 {
			return '?'
		}
		return r
	}, s) + "\""
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
