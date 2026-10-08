VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint docker dev clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired ./cmd/fired
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired-agent ./cmd/fired-agent

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

clean:
	rm -rf bin .dev
