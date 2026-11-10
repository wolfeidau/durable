FROM golang:1.27.3-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /out/durable-worker ./cmd/durable-worker

FROM debian:13.7-slim
RUN apt-get update && apt-get install -y --no-install-recommends bash git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/durable-worker /usr/local/bin/durable-worker

# OpenShell starts the supervisor as root and drops agent children to this user.
RUN groupadd --gid 1500 app \
    && useradd --uid 1500 --gid app --create-home app \
    && install -d -o app -g app /sandbox
WORKDIR /sandbox
USER app
CMD ["/usr/local/bin/durable-worker"]
