VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint docker dev dev-artifacts clean try

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired ./cmd/fired
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired-agent ./cmd/fired-agent
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired-artifacts ./cmd/fired-artifacts

test:
	go test ./...

race:
	go test -race ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

docker:
	docker build --build-arg VERSION=$(VERSION) -t fired:$(VERSION) -t fired:latest .

# Run the daemon locally with the fake driver (no KVM, NO isolation).
dev: build
	FIRED_API_TOKEN=dev FIRED_DRIVER=fake FIRED_DATA_DIR=$(CURDIR)/.dev \
	  ./bin/fired serve -config deploy/dev.yaml

# Run the artifact service against `make dev` (run that first, in another
# terminal), with its console on http://127.0.0.1:8444/ui/ (token "dev"). Jobs use fired's "base" template: create it with
#   curl -H "X-Fired-Token: dev" -d '{"name":"base"}' http://127.0.0.1:8443/v1/templates
dev-artifacts: build
	FIRED_ARTIFACTS_TOKEN=dev FIRED_ARTIFACTS_DATA_DIR=$(CURDIR)/.dev/artifacts FIRED_ARTIFACTS_UI=1 \
	  FIRED_ARTIFACTS_LISTEN=127.0.0.1:8444 \
	  FIRED_URL=http://127.0.0.1:8443 FIRED_API_TOKEN=dev \
	  ./bin/fired-artifacts serve -config deploy/artifacts.yaml

clean:
	rm -rf bin .dev

# Run the production image with nothing mounted: real Firecracker VMs, the
# web console on http://localhost:8443/ui/ (token below), and all state
# inside the container, so `docker rm` (or Ctrl-C, thanks to --rm) wipes it.
TRY_PORT ?= 8443
TRY_TOKEN ?= dev
try: docker
	docker run --rm -it --name fired-try --privileged --cgroupns=private \
	  --tmpfs /data/hot:exec,dev,mode=0755,size=8g \
	  -p 127.0.0.1:$(TRY_PORT):8443 \
	  -e FIRED_API_TOKEN=$(TRY_TOKEN) -e FIRED_UI=1 \
	  fired:latest
