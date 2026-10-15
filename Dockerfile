# ---- Stage 1: build -------------------------------------------------------
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Dependencies first: cached as long as go.mod/go.sum are unchanged.
COPY go.mod go.sum ./
RUN go mod download

# Then the source tree (see .dockerignore for what stays out).
COPY . .

# Pure-Go SQLite (modernc.org/sqlite) -> CGO_ENABLED=0 static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/runebot ./cmd/runebot

# ---- Stage 2: runtime ------------------------------------------------------
# Runtime image must carry the attachment tools the agent shells out to,
# plus TLS roots. No secrets baked in; config via env (RUNE_LISTEN, RUNE_TOKENS).
FROM alpine:3.20

RUN apk add --no-cache \
        ca-certificates \
        poppler-utils \
        poppler-data \
        mupdf-tools \
    && apk add --no-cache --repository=https://dl-cdn.alpinelinux.org/alpine/edge/testing \
        catdoc \
    && adduser -D -u 1000 rune \
    && install -d -o rune -g rune /home/rune/.rune

COPY --from=builder /out/runebot /runebot

USER rune

# Data lives under ~/.rune/* ; mount a volume at /home/rune/.rune for persistence.
EXPOSE 8765

ENTRYPOINT ["/runebot"]
