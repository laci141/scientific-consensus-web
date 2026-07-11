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
2. Paste claim + your LLM key (Anthropic/OpenAI/Gemini/etc.)
3. Click a button → results in modal

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
