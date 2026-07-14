# Deployment Guide

## Local Docker Build (requires Docker installed)

```bash
cd /c/Users/LACI/scientific-consensus-web

# Build the image (multi-stage: builds web server on Linux + uses vendored CLI)
docker build -t scientific-consensus-web:latest .

# Run locally
docker run -p 8090:8090 scientific-consensus-web:latest

# Test
curl http://127.0.0.1:8090/
curl http://127.0.0.1:8090/api/consensus -X POST \
  -H "Content-Type: application/json" \
  -d '{"claim":"vitamin D reduces infections","limit":10}'
```

> The runtime image copies the pre-built `bin/scientific-consensus-pp-cli-linux`.
> Regenerate it with `./vendor-cli.sh` whenever the CLI source changes.

## Render Deployment

### Prerequisites
- GitHub repo pushed: https://github.com/laci141/scientific-consensus-web
- Render account: https://render.com (free tier OK)

### Step 1: Create Render Web Service

1. Sign in to https://render.com
2. Click "New +" → "Web Service"
3. Connect GitHub → Select `scientific-consensus-web`
4. Name: `scientific-consensus-web` (or custom)
5. Environment: Docker
6. Build command: `docker build -t app .` (leave blank for auto-detect)
7. Start command: (leave blank — Dockerfile CMD runs)
8. Port: `8090`
9. Plan: Free (512 MB RAM, spins down after 15 min) or Starter ($7/mo, always on)
10. No environment variables needed (BYOK via headers)
11. Click "Create Web Service"

### Step 2: Wait for Render Deploy

Render clones, builds (`docker build`), and deploys. Watch the "Deploy" tab for logs.

URL format: `https://scientific-consensus-web-<random>.onrender.com`

### Step 3: Verify on Render

```bash
RENDER_URL="https://scientific-consensus-web-<your-random>.onrender.com"

curl $RENDER_URL/
curl -X POST $RENDER_URL/api/consensus \
  -H "Content-Type: application/json" \
  -d '{"claim":"coffee improves alertness","limit":15}'
```

### Step 4: Share

1. Open `$RENDER_URL` in browser
2. Paste claim + your LLM key (any provider below)
3. Click a button → results in modal

## BYOK LLM Providers

The CLI itself is keyless/heuristic. When you supply a key (`X-LLM-Key` header,
never stored or logged), the web layer makes ONE chat call to your chosen
provider to synthesize the CLI output into a structured verdict
(`llm_synthesis`: stance / confidence / reasoning / key evidence). If the LLM
call fails you still get the full heuristic result plus a redacted `llm_error`.

| provider | base_url | default model | get a key |
|---|---|---|---|
| `anthropic` | api.anthropic.com/v1 (native Messages API) | claude-haiku-4-5 | console.anthropic.com |
| `openai` | api.openai.com/v1 | gpt-5-mini | platform.openai.com |
| `gemini` | generativelanguage.googleapis.com/v1beta/openai | gemini-2.5-flash | aistudio.google.com/apikey |
| `groq` | api.groq.com/openai/v1 | llama-3.3-70b-versatile | console.groq.com |
| `mistral` | api.mistral.ai/v1 | mistral-small-latest | console.mistral.ai |
| `deepseek` | api.deepseek.com | deepseek-chat | platform.deepseek.com |
| `zai` | api.z.ai/api/paas/v4 | glm-5 | z.ai/model-api |
| `moonshot` | api.moonshot.ai/v1 | kimi-k2.6 | platform.moonshot.ai |
| `qwen` | dashscope-intl.aliyuncs.com/compatible-mode/v1 | qwen3-max | Alibaba Cloud Model Studio |
| `minimax` | api.minimax.io/v1 | MiniMax-M2.7 | platform.minimax.io |
| `xai` | api.x.ai/v1 | grok-4-fast | console.x.ai |
| `openrouter` | openrouter.ai/api/v1 | deepseek/deepseek-chat | openrouter.ai/keys |

All providers except `anthropic` speak the OpenAI chat-completions format.
`openrouter` is a meta-provider: the optional `model` body field selects any
hosted model (including `:free` ones), so always set it there. `model` is an
opaque token: trimmed, max 128 chars, no whitespace/control characters.

```bash
# 1. Heuristic (no key) — CLI result only
curl -X POST $RENDER_URL/api/consensus \
  -H "Content-Type: application/json" \
  -d '{"claim":"vitamin D reduces infections","limit":20}'

# 2. DeepSeek synthesis (default model deepseek-chat)
curl -X POST $RENDER_URL/api/consensus \
  -H "Content-Type: application/json" \
  -H "X-LLM-Key: sk-your-deepseek-key" \
  -d '{"claim":"vitamin D reduces infections","provider":"deepseek","limit":20}'

# 3. OpenRouter with an explicit (free) model
curl -X POST $RENDER_URL/api/consensus \
  -H "Content-Type: application/json" \
  -H "X-LLM-Key: sk-or-your-openrouter-key" \
  -d '{"claim":"vitamin D reduces infections","provider":"openrouter","model":"deepseek/deepseek-chat-v3-0324:free","limit":20}'
```

## Troubleshooting

**"Cannot find CLI binary"**
- Ensure `bin/scientific-consensus-pp-cli-linux` exists before `docker build`.
- If missing, re-run vendoring + cross-compile: `./vendor-cli.sh`.

**"Port 8090 not accessible"**
- Render assigns a `PORT` env var (not necessarily 8090).
- The server reads `PORT` and binds `0.0.0.0:$PORT` automatically.
- Test: `curl $RENDER_URL/healthz` (should return `ok`).

**"Free tier spins down after 15 min idle"**
- That's normal. Render wakes it on next request (~30s cold start).
- Upgrade to Starter ($7/mo) for always-on.
