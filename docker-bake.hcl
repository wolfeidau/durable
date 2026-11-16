# The derived images take the worker build as a context, because a remote builder can't see a locally loaded image.
group "default" {
  targets = ["worker", "worker-test", "worker-go"]
}

target "worker" {
  dockerfile = "docker/worker.Dockerfile"
  tags       = ["durable-worker:dev"]
}

target "worker-test" {
  dockerfile = "docker/worker-test.Dockerfile"
  contexts   = { "durable-worker:dev" = "target:worker" }
  tags       = ["durable-worker:mcptest"]
}

target "worker-go" {
  dockerfile = "docker/worker-go.Dockerfile"
  contexts   = { "durable-worker:dev" = "target:worker" }
  tags       = ["durable-worker-go:dev"]
}
