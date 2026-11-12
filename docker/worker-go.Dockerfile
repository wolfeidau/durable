# syntax=docker/dockerfile:1
FROM golang:1.27.3-trixie AS go
FROM durable-worker:dev
ARG TARGETARCH
USER root
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
# golangci-lint 2.15.0, matching mise.toml; checksums from its release checksums.txt
RUN case "$TARGETARCH" in \
      amd64) sum=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea ;; \
      arm64) sum=b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/gl.tgz "https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-linux-${TARGETARCH}.tar.gz" \
 && echo "$sum  /tmp/gl.tgz" | sha256sum -c - \
 && tar -xzf /tmp/gl.tgz -C /tmp \
 && mv "/tmp/golangci-lint-2.15.0-linux-${TARGETARCH}/golangci-lint" /usr/local/bin/ \
 && rm -rf /tmp/gl.tgz "/tmp/golangci-lint-2.15.0-linux-${TARGETARCH}"
# HOME is the repo workspace in the sandbox, so caches live in /tmp to stay out of commits.
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org GOPATH=/tmp/go GOCACHE=/tmp/go-build GOLANGCI_LINT_CACHE=/tmp/golangci-lint XDG_CACHE_HOME=/tmp/.cache
USER app
WORKDIR /sandbox
