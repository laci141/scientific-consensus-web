# syntax=docker/dockerfile:1
#
# Multi-stage build for scientific-consensus-web.
#
# Stage 1 builds the web server for linux/amd64 from all root *.go files
# (main.go + providers.go).
# The runtime stage copies in a PRE-BUILT linux/amd64 CLI binary,
# bin/scientific-consensus-pp-cli-linux, produced by vendor-cli.sh (which
# cross-compiles the scientific-consensus CLI from the monorepo source). That
# binary must exist in the build context before `docker build` — a Windows .exe
# cannot run in this Linux image, which is why we ship a Linux build.

# ---- Stage 1: build the web server ------------------------------------------
# The web module is stdlib-only, so it has no go.sum and `go mod download` is a
# no-op — copy just go.mod.
FROM golang:1.26-alpine AS web-builder
WORKDIR /build
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/server .

# ---- Stage 2: minimal runtime ------------------------------------------------
FROM alpine:latest
# ca-certificates: the CLI makes HTTPS calls to PubMed/OpenAlex/Crossref and,
# when a BYOK key is supplied, to the LLM providers.
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
WORKDIR /app
COPY --from=web-builder /out/server ./server
COPY bin/scientific-consensus-pp-cli-linux ./bin/scientific-consensus-pp-cli
COPY index.html ./index.html
RUN chmod +x ./bin/scientific-consensus-pp-cli
ENV CLI_BIN=/app/bin/scientific-consensus-pp-cli
# The server binds 0.0.0.0:$PORT when Render sets $PORT; locally it defaults to
# 127.0.0.1:8090.
EXPOSE 8090
USER app
CMD ["./server"]
