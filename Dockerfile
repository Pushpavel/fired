# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fired ./cmd/fired \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fired-agent ./cmd/fired-agent

FROM debian:bookworm-slim
ARG FIRECRACKER_VERSION=v1.16.2
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl e2fsprogs \
 && rm -rf /var/lib/apt/lists/*
RUN set -eux; arch="$(uname -m)"; \
    curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${arch}.tgz" \
      | tar -xz -C /tmp; \
    rel="/tmp/release-${FIRECRACKER_VERSION}-${arch}"; \
    install -m 0755 "${rel}/firecracker-${FIRECRACKER_VERSION}-${arch}" /usr/local/bin/firecracker; \
    install -m 0755 "${rel}/jailer-${FIRECRACKER_VERSION}-${arch}" /usr/local/bin/jailer; \
    rm -rf /tmp/release-*; \
    firecracker --version
COPY --from=build /out/fired /usr/local/bin/fired
COPY --from=build /out/fired-agent /usr/local/lib/fired/fired-agent
COPY deploy/config.yaml /etc/fired/config.yaml
ENV FIRED_CONFIG=/etc/fired/config.yaml
EXPOSE 8443
STOPSIGNAL SIGTERM
ENTRYPOINT ["fired"]
CMD ["serve"]
