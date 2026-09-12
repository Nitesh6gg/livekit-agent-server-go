# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# Two separate CGO dependencies live in this binary: the LiveKit media path
# binds to libopus (docs/DECISIONS.md ADR-005), and internal/audio binds to
# sherpa-onnx for VAD and speech enhancement (ADR-018). Both need Linux amd64
# with CGO enabled.
FROM golang:1.26-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        libopus-dev \
        libopusfile-dev \
        libsoxr-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1 GOOS=linux GOARCH=amd64
RUN go build -o /out/agent ./cmd/agent

# sherpa-onnx ships prebuilt shared libraries inside its module, including its
# own bundled onnxruntime — which is why there is no separate onnxruntime stage
# here any more, and nothing to keep version-matched against a Go binding.
# Globbed rather than version-pinned so a module bump does not silently produce
# an image whose binary cannot start.
RUN mkdir -p /out/lib && \
    cp "$(go env GOMODCACHE)"/github.com/k2-fsa/sherpa-onnx-go-linux@*/lib/x86_64-unknown-linux-gnu/*.so /out/lib/

# ---- runtime stage ----------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        libopus0 \
        libopusfile0 \
        libsoxr0 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# libsherpa-onnx-c-api.so is linked, not dlopened: without this the binary does
# not start at all.
COPY --from=build /out/lib/ /usr/local/lib/
RUN ldconfig

WORKDIR /app
COPY --from=build /out/agent ./agent
COPY models/ ./models/

ENV VAD_MODEL_PATH=/app/models/ten-vad.onnx
ENV GTCRN_MODEL_PATH=/app/models/gtcrn_simple.onnx

# Config is supplied at runtime via --env-file / -e, not baked into the image.
EXPOSE 8080

ENTRYPOINT ["./agent"]
