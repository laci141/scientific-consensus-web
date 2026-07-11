// Command scientific-consensus-web is a thin standalone HTTP wrapper around the
// scientific-consensus CLI. Its single job (Phase 1) is to expose one endpoint,
// POST /api/consensus, that runs the compiled CLI once per request and injects a
// per-request BYOK (bring-your-own-key) provider API key ONLY into that child
// process's environment. Each caller therefore uses their own LLM key; the
// server never holds a key of its own.
//
// SECURITY MODEL (enforced below, do not weaken):
//   - The BYOK key arrives in the X-LLM-Key request header and lives in memory
//     only for the duration of one request and one child process.
//   - The key is NEVER logged, printed, persisted, or written to the server's
//     own process environment. buildChildEnv() strips every known provider key
//     out of os.Environ() before adding the single per-request key, so a key set
//     in the server's own environment can never leak into a caller's request and
//     a caller's key can never outlive the child.
//   - Any CLI stderr surfaced to the client is passed through redact(), which
//     removes the key substring so a key echoed in an error can never escape.
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

// providerEnvVar maps a BYOK provider name to the environment variable the CLI
// reads for that provider. The CLI checks these in a fixed priority order; by
// setting exactly one (and stripping all others) we make the caller's chosen
// provider deterministic. Empty provider => keyless heuristic run.
var providerEnvVar = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
	"gemini":    "GEMINI_API_KEY",
	"groq":      "GROQ_API_KEY",
	"mistral":   "MISTRAL_API_KEY",
}

// allProviderEnvVars is every provider key the CLI might read. buildChildEnv
// strips ALL of them from the inherited environment so the only provider key the
// child ever sees is the one supplied on this request.
var allProviderEnvVars = func() []string {
	out := make([]string, 0, len(providerEnvVar))
	for _, v := range providerEnvVar {
		out = append(out, v)
	}
	return out
}()

type consensusRequest struct {
	Claim    string `json:"claim"`
	Provider string `json:"provider"`
	Limit    int    `json:"limit"`
}

// consensusResponse wraps the CLI's raw JSON verbatim and adds one echoed field,
// stance_source, describing what the web layer *requested*: "llm" when a key was
// supplied (so the LLM path was attempted), "heuristic" when it was not. The
// authoritative record of what actually ran is the CLI's own stance_method field
// inside Result ("llm:<provider>" on success, "heuristic" on fallback/no-key).
type consensusResponse struct {
	StanceSource string          `json:"stance_source"`
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

func handleRoot(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html":
		// Serve the single-page frontend from the same origin as /api/* so the
		// browser fetch needs no CORS relaxation. Falls back to "ok" (a plain
		// health check) when index.html isn't present next to the binary.
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

// byok holds the per-request BYOK decision derived from the request headers:
// which env var to set for the child (envKeyVar), the key value, and the
// stanceSource label echoed to the caller ("llm" or "heuristic").
type byok struct {
	envKeyVar    string
	key          string
	stanceSource string
}

// extractBYOK reads the X-LLM-Key header and resolves the provider (from the
// bodyProvider argument, falling back to the X-LLM-Provider header). It returns
// the byok decision and true on success; on a client error it writes the
// response and returns false so the caller stops. When no key is supplied it
// succeeds with a heuristic (keyless) decision.
func extractBYOK(w http.ResponseWriter, r *http.Request, bodyProvider string) (byok, bool) {
	// Key from header only (never from body — a key in a JSON body is easier to
	// accidentally log).
	key := strings.TrimSpace(r.Header.Get("X-LLM-Key"))
	if key == "" {
		return byok{stanceSource: "heuristic"}, true
	}
	provider := strings.ToLower(strings.TrimSpace(bodyProvider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(r.Header.Get("X-LLM-Provider")))
	}
	if provider == "" {
		writeError(w, http.StatusBadRequest, "X-LLM-Key supplied but no provider; set \"provider\" in body or X-LLM-Provider header")
		return byok{}, false
	}
	v, ok := providerEnvVar[provider]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider "+quoteToken(provider)+"; supported: anthropic, openai, gemini, groq, mistral")
		return byok{}, false
	}
	return byok{envKeyVar: v, key: key, stanceSource: "llm"}, true
}

// clampLimit normalizes a caller-supplied --limit into the CLI's accepted range,
// defaulting to def when out of bounds.
func clampLimit(limit, def int) int {
	if limit <= 0 || limit > 200 {
		return def
	}
	return limit
}

// runCLIJSON runs the CLI with the given argv (subcommand + positional args +
// flags already assembled by the caller), injecting the BYOK key into ONLY the
// child process environment, and writes the CLI's JSON stdout back to the client
// wrapped with the echoed stance_source. It centralizes the exec, timeout,
// key-redaction, and JSON-validation shared by every endpoint.
func runCLIJSON(w http.ResponseWriter, r *http.Request, b byok, args []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// #nosec G204 -- args are fixed subcommands/flags plus user text as discrete
	// argv elements (no shell), and the key goes only into the child's env.
	cmd := exec.CommandContext(ctx, cliBinaryPath(), args...)
	cmd.Env = buildChildEnv(b.envKeyVar, b.key)

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
		StanceSource: b.stanceSource,
		Result:       json.RawMessage(raw),
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

// claimRequest is the shared body for the single-claim subcommands. Provider is
// only meaningful for LLM-capable subcommands (consensus); it is harmless on the
// others since a key simply selects the child env var.
type claimRequest struct {
	Claim    string `json:"claim"`
	Provider string `json:"provider"`
	Limit    int    `json:"limit"`
}

// handleClaimCmd is the shared handler for every subcommand whose CLI shape is
// "<subcommand> <claim> --json --limit <n>". defLimit is that subcommand's CLI
// default, used when the caller omits or over-ranges limit.
func handleClaimCmd(w http.ResponseWriter, r *http.Request, subcommand string, defLimit int) {
	var req claimRequest
	if !decodePOST(w, r, &req) {
		return
	}
	req.Claim = strings.TrimSpace(req.Claim)
	if req.Claim == "" {
		writeError(w, http.StatusBadRequest, "claim is required")
		return
	}
	b, ok := extractBYOK(w, r, req.Provider)
	if !ok {
		return
	}
	args := []string{subcommand, req.Claim, "--json", "--limit", fmt.Sprintf("%d", clampLimit(req.Limit, defLimit))}
	runCLIJSON(w, r, b, args)
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
	Limit    int    `json:"limit"`
}

func handleCompare(w http.ResponseWriter, r *http.Request) {
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
	b, ok := extractBYOK(w, r, req.Provider)
	if !ok {
		return
	}
	args := []string{"compare", req.Claim1, req.Claim2, "--json", "--limit", fmt.Sprintf("%d", clampLimit(req.Limit, 40))}
	runCLIJSON(w, r, b, args)
}

// buildChildEnv returns the environment for the child CLI process: the server's
// own environment with EVERY provider key removed, plus (when keyVar != "") the
// single per-request key. This guarantees the child sees exactly zero or one
// provider key, and never one that belongs to the server itself.
func buildChildEnv(keyVar, keyVal string) []string {
	strip := make(map[string]struct{}, len(allProviderEnvVars))
	for _, v := range allProviderEnvVars {
		strip[v] = struct{}{}
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
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
	if keyVar != "" && keyVal != "" {
		out = append(out, keyVar+"="+keyVal)
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
