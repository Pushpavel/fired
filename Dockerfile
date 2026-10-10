# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fired ./cmd/fired \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fired-agent ./cmd/fired-agent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fired-artifacts ./cmd/fired-artifacts

# Guest kernel, built from Firecracker's CI config plus what fired needs built
# in (overlayfs root, vsock, ext4, devtmpfs, virtio-rng, cgroup v2). Cached
# by BuildKit after the first build. scripts/build-kernel.sh exports this
# stage on its own.
FROM debian:bookworm AS kernel
ARG KERNEL_VERSION=6.1.140
ARG FIRECRACKER_VERSION=v1.16.2
RUN apt-get update -qq \
 && apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev curl xz-utils ca-certificates >/dev/null
WORKDIR /build
RUN set -eux; arch="$(uname -m)"; \
    curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" | tar -xJ; \
    cd "linux-${KERNEL_VERSION}"; \
    curl -fsSL "https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FIRECRACKER_VERSION}/resources/guest_configs/microvm-kernel-ci-${arch}-6.1.config" -o .config; \
    for opt in OVERLAY_FS VSOCKETS VIRTIO_VSOCKETS VIRTIO_VSOCKETS_COMMON EXT4_FS EXT4_FS_POSIX_ACL \
               DEVTMPFS DEVTMPFS_MOUNT TMPFS TMPFS_POSIX_ACL UNIX98_PTYS HW_RANDOM HW_RANDOM_VIRTIO \
               CGROUPS MEMCG CGROUP_PIDS CGROUP_SCHED FAIR_GROUP_SCHED CPUSETS \
               VIRTIO_BLK VIRTIO_MMIO SECCOMP FUTEX EPOLL INOTIFY_USER FANOTIFY UNIX INET IPV6; do \
      scripts/config --enable "$opt"; \
    done; \
    make olddefconfig >/dev/null; \
    for opt in OVERLAY_FS VIRTIO_VSOCKETS EXT4_FS DEVTMPFS; do \
      grep -q "^CONFIG_${opt}=y" .config || { echo "CONFIG_${opt} is not built in"; exit 1; }; \
    done; \
    mkdir -p /out; \
    if [ "$arch" = "aarch64" ]; then make -j"$(nproc)" Image >/dev/null; cp arch/arm64/boot/Image /out/vmlinux; \
    else make -j"$(nproc)" vmlinux >/dev/null; cp vmlinux /out/vmlinux; fi; \
    cp .config /out/vmlinux.config

FROM scratch AS kernel-export
COPY --from=kernel /out/ /

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
# The artifact service ships in the same image; run it with
# --entrypoint fired-artifacts (see docs/artifacts.md).
COPY --from=build /out/fired-artifacts /usr/local/bin/fired-artifacts
# Default guest kernel. Mounting a directory over /data/kernels replaces it.
COPY --from=kernel /out/vmlinux /data/kernels/vmlinux
COPY deploy/config.yaml /etc/fired/config.yaml
ENV FIRED_CONFIG=/etc/fired/config.yaml
EXPOSE 8443
STOPSIGNAL SIGTERM
ENTRYPOINT ["fired"]
CMD ["serve"]
