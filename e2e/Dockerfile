FROM golang:1.27.3-trixie

RUN useradd --create-home --uid 10001 tester \
    && mkdir -p /src /home/tester/.cache /go/pkg \
    && chown -R tester:tester /src /home/tester /go
USER tester
WORKDIR /src

ENV GOFLAGS=-race GOCACHE=/home/tester/.cache/go-build

COPY --chown=tester:tester go.mod go.sum ./
RUN go mod download

COPY --chown=tester:tester . .
# Warm the build cache while the network is available; runtime has none.
RUN go test -race -count=1 -run '^$' ./... && go build -race -o /dev/null ./cmd/durable

ENV GOFLAGS="-race -mod=readonly" GOPROXY=off GOTOOLCHAIN=local

COPY --chown=tester:tester e2e/egress-check.sh /usr/local/bin/egress-check.sh
ENTRYPOINT ["/usr/local/bin/egress-check.sh"]
CMD ["go", "test", "-race", "-count=1", "./cmd/durable"]
