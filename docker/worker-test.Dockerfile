FROM golang:1.27.3-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/mcptest-server ./internal/testdata/mcpserver

# The worker image plus a stdio MCP server for the gateway tests.
FROM durable-worker:dev
COPY --from=build /out/mcptest-server /usr/local/bin/mcptest-server
