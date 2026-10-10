# Update digests and tool versions through a reviewed PR. No credential build args.
FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/rein ./cmd/rein

# Official Node 22 LTS image is based on Debian bookworm-slim.
FROM node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
ARG CLAUDE_CODE_VERSION=2.1.296
ARG CODEX_VERSION=0.162.1
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git gh \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && groupadd --gid 10001 rein \
    && useradd --uid 10001 --gid rein --create-home --shell /bin/sh rein \
    && mkdir -p /etc/rein /home/rein/work \
    && chown rein:rein /home/rein/work
COPY --from=build /out/rein /usr/local/bin/rein
ENV HOME=/home/rein REIN_HOME=/home/rein/.rein
USER 10001:10001
WORKDIR /home/rein/work
# Mount a deployment-specific, credential-free config at this path.
ENTRYPOINT ["rein", "--config", "/etc/rein/config.toml", "run", "--hosted", "--once"]
