# syntax=docker/dockerfile:1
FROM golang:1.27.3-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /out/durable-worker ./cmd/durable-worker

FROM debian:13.7-slim
RUN apt-get update && apt-get install -y --no-install-recommends bash git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
# gh 2.104.0; checksums from its release gh_2.104.0_checksums.txt
RUN case "$TARGETARCH" in \
      amd64) sum=98cb101ad87dbbd3f6b2f2bfcbd43197ccd07643140121d8a9cfaa91eadbc24e ;; \
      arm64) sum=cb34cefc6f4e77b90731374a2b737516bb8f3033c6ca46aa949e5344f68069e3 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v2.104.0/gh_2.104.0_linux_${TARGETARCH}.tar.gz" \
 && echo "$sum  /tmp/gh.tgz" | sha256sum -c - \
 && tar -xzf /tmp/gh.tgz -C /tmp \
 && install -m 0755 "/tmp/gh_2.104.0_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh \
 && rm -rf /tmp/gh.tgz "/tmp/gh_2.104.0_linux_${TARGETARCH}"
# HOME is the workspace, so gh keeps its config in /tmp to stay out of commits.
ENV GH_CONFIG_DIR=/tmp/gh GH_NO_UPDATE_NOTIFIER=1 GH_PROMPT_DISABLED=1 GH_SPINNER_DISABLED=1
# mise 2026.10.25; checksums from its release SHASUMS256.txt
RUN case "$TARGETARCH" in \
      amd64) arch=x64; sum=199966edaad1b208eb4c72955f1722dafb8fa799f575db720dea05aaa8216546 ;; \
      arm64) arch=arm64; sum=a7d1e2add53441ff76e7e971e7a3fb15c117741836ce51c026f5fb7eebe7dbcf ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/mise.tgz "https://github.com/jdx/mise/releases/download/v2026.10.25/mise-v2026.10.25-linux-${arch}.tar.gz" \
 && echo "$sum  /tmp/mise.tgz" | sha256sum -c - \
 && tar -xzf /tmp/mise.tgz -C /tmp \
 && install -m 0755 /tmp/mise/bin/mise /usr/local/bin/mise \
 && rm -rf /tmp/mise.tgz /tmp/mise
# HOME is the workspace, so mise keeps its state in /tmp. MISE_LOCKED makes installs use the lockfile's URLs and never look versions up; the versions host is off because the grant refuses it.
ENV MISE_DATA_DIR=/tmp/mise/data MISE_CACHE_DIR=/tmp/mise/cache MISE_STATE_DIR=/tmp/mise/state MISE_LOCKED=1 MISE_USE_VERSIONS_HOST=0 MISE_YES=1 MISE_TRUSTED_CONFIG_PATHS=/sandbox PATH=/tmp/mise/data/shims:$PATH
COPY --from=build /out/durable-worker /usr/local/bin/durable-worker

# OpenShell starts the supervisor as root and drops agent children to this user.
RUN groupadd --gid 1500 app \
    && useradd --uid 1500 --gid app --create-home app \
    && install -d -o app -g app /sandbox
WORKDIR /sandbox
USER app
CMD ["/usr/local/bin/durable-worker"]
