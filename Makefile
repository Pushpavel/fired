VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# fired keeps its state in Postgres. `make db` runs one for development and
# tests in a container (data in a docker volume; `make clean-db` drops it).
DB_PORT ?= 5434
DB_URL  ?= postgres://fired:fired@127.0.0.1:$(DB_PORT)/fired?sslmode=disable
PG_IMAGE ?= postgres:16-alpine

.PHONY: build test race lint docker db clean-db dev dev-vm dev-artifacts clean try

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired ./cmd/fired
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired-agent ./cmd/fired-agent
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fired-artifacts ./cmd/fired-artifacts

# Tests that need a database create their own in this server and drop it.
test: db
	FIRED_TEST_DATABASE_URL="$(DB_URL)" go test ./...

race: db
	FIRED_TEST_DATABASE_URL="$(DB_URL)" go test -race ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

docker:
	docker build --build-arg VERSION=$(VERSION) -t fired:$(VERSION) -t fired:latest .

db:
	@docker inspect -f '{{.State.Running}}' fired-dev-db 2>/dev/null | grep -q true || { \
	  docker rm -f fired-dev-db >/dev/null 2>&1; \
	  docker run -d --name fired-dev-db -p 127.0.0.1:$(DB_PORT):5432 \
	    -e POSTGRES_USER=fired -e POSTGRES_PASSWORD=fired -e POSTGRES_DB=fired \
	    -v fired-dev-db:/var/lib/postgresql/data $(PG_IMAGE) >/dev/null; }
	@until docker exec fired-dev-db pg_isready -q -U fired; do sleep 0.5; done

clean-db:
	docker rm -f fired-dev-db
	docker volume rm fired-dev-db

# Run the daemon locally with the fake driver (no KVM, NO isolation).
dev: build db
	FIRED_API_TOKEN=dev FIRED_DRIVER=fake FIRED_DATA_DIR=$(CURDIR)/.dev FIRED_DATABASE_URL="$(DB_URL)" \
	  ./bin/fired serve -config deploy/dev.yaml

# Run the daemon locally with real Firecracker VMs (needs /dev/kvm), from the
# image, on the host network (127.0.0.1:8443, token "dev", console at /ui/).
# Templates and archives persist in docker volumes; the hot tier is a
# tmpfs, so sandboxes are archived when it stops. Detached: `docker logs -f
# fired-dev`, `docker stop fired-dev`.
dev-vm: docker db
	# stop (not rm -f): fired archives its sandboxes on the way down.
	docker stop fired-dev >/dev/null 2>&1 || true
	docker rm -f fired-dev >/dev/null 2>&1 || true
	docker run -d --name fired-dev --privileged --cgroupns=private --network host \
	  --add-host fired-artifacts.internal:127.0.0.1 \
	  --tmpfs /data/hot:exec,dev,mode=0755,size=8g \
	  -v fired-dev-cold:/data/cold -v fired-dev-templates:/data/templates \
	  -v $(CURDIR)/deploy/dev-vm.yaml:/etc/fired/config.yaml:ro \
	  -e FIRED_API_TOKEN=dev -e FIRED_HOST_ID=dev-vm -e FIRED_ADVERTISE_URL=http://127.0.0.1:8443 -e FIRED_DATABASE_URL="$(DB_URL)" \
	  --stop-timeout 120 fired:latest

# Run the artifact service from the image, as its own container the way it's
# its own Deployment in Kubernetes, against `make dev-vm` or `make dev` (run
# that first), with its console on http://127.0.0.1:8444/ui/ (token "dev").
# Its records are in the dev Postgres (`make db`, schema "artifacts") and
# its blobs in .dev/artifacts. Jobs reach its db through fired's egress
# proxy at fired-artifacts.internal:8444: a name (sandboxes don't proxy
# loopback addresses) that fired's container maps to 127.0.0.1 (make
# dev-vm), which deploy/dev-vm.yaml lets fired reach.
# Detached: `docker logs -f fired-dev-artifacts`, `docker stop fired-dev-artifacts`.
# Jobs use fired's "base" template: create it with
#   curl -H "X-Fired-Token: dev" -d '{"name":"base"}' http://127.0.0.1:8443/v1/templates
dev-artifacts: docker db
	mkdir -p $(CURDIR)/.dev/artifacts
	docker rm -f fired-dev-artifacts >/dev/null 2>&1 || true
	docker run -d --name fired-dev-artifacts --network host --user $$(id -u):$$(id -g) \
	  -v $(CURDIR)/deploy/artifacts.yaml:/etc/fired-artifacts/artifacts.yaml:ro \
	  -v $(CURDIR)/.dev/artifacts:/data/artifacts \
	  -e FIRED_ARTIFACTS_TOKEN=dev -e FIRED_ARTIFACTS_UI=1 -e FIRED_ARTIFACTS_LISTEN=127.0.0.1:8444 \
	  -e FIRED_ARTIFACTS_DATABASE_URL="$(DB_URL)" -e FIRED_ARTIFACTS_DB_URL=http://fired-artifacts.internal:8444 \
	  -e FIRED_URL=http://127.0.0.1:8443 -e FIRED_API_TOKEN=dev \
	  --entrypoint fired-artifacts fired:latest serve -config /etc/fired-artifacts/artifacts.yaml

clean:
	rm -rf bin .dev

# Run the production image with nothing mounted: real Firecracker VMs, the
# web console on http://localhost:8443/ui/ (token below), and all state
# inside containers (fired, and a Postgres next to it on a private network),
# so Ctrl-C wipes it.
TRY_PORT ?= 8443
TRY_TOKEN ?= dev
try: docker
	docker network create fired-try >/dev/null 2>&1 || true
	docker rm -f fired-try-db >/dev/null 2>&1 || true
	docker run -d --rm --name fired-try-db --network fired-try \
	  -e POSTGRES_USER=fired -e POSTGRES_PASSWORD=fired -e POSTGRES_DB=fired $(PG_IMAGE) >/dev/null
	docker run --rm -it --name fired-try --privileged --cgroupns=private --network fired-try \
	  --tmpfs /data/hot:exec,dev,mode=0755,size=8g \
	  -p 127.0.0.1:$(TRY_PORT):8443 \
	  -e FIRED_API_TOKEN=$(TRY_TOKEN) -e FIRED_UI=1 \
	  -e FIRED_DATABASE_URL="postgres://fired:fired@fired-try-db:5432/fired?sslmode=disable" \
	  fired:latest; \
	  docker rm -f fired-try-db >/dev/null
