#!/usr/bin/env bash
# Builds an uncompressed guest kernel (vmlinux) for fired sandboxes.
#
# Starts from Firecracker's own CI guest config and enables what fired
# needs built in (no modules are loaded in the guest):
#   overlayfs (writable root over the read-only template), vsock,
#   ext4, devtmpfs, virtio-rng, cgroup v2.
#
# Usage: scripts/build-kernel.sh [output-dir]      (needs Docker)
#   KERNEL_VERSION=6.1.140 FIRECRACKER_REF=v1.16.2 scripts/build-kernel.sh /mnt/hdd/fired/kernels
set -euo pipefail

OUT="${1:-$PWD/out}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.140}"
FIRECRACKER_REF="${FIRECRACKER_REF:-v1.16.2}"
ARCH="$(uname -m)"
CONFIG_URL="${CONFIG_URL:-https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FIRECRACKER_REF}/resources/guest_configs/microvm-kernel-ci-${ARCH}-6.1.config}"

mkdir -p "$OUT"
echo "building linux ${KERNEL_VERSION} for ${ARCH} with config ${CONFIG_URL}"

docker run --rm -i -v "$OUT:/out" \
  -e KV="$KERNEL_VERSION" -e CONFIG_URL="$CONFIG_URL" -e ARCH_NAME="$ARCH" \
  debian:bookworm bash -euxo pipefail <<'INNER'
apt-get update -qq
apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev curl xz-utils ca-certificates >/dev/null
cd /tmp
curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KV}.tar.xz" | tar -xJ
cd "linux-${KV}"
curl -fsSL "$CONFIG_URL" -o .config
for opt in OVERLAY_FS VSOCKETS VIRTIO_VSOCKETS VIRTIO_VSOCKETS_COMMON EXT4_FS EXT4_FS_POSIX_ACL \
           DEVTMPFS DEVTMPFS_MOUNT TMPFS TMPFS_POSIX_ACL UNIX98_PTYS HW_RANDOM HW_RANDOM_VIRTIO \
           CGROUPS MEMCG CGROUP_PIDS CGROUP_SCHED FAIR_GROUP_SCHED CPUSETS \
           VIRTIO_BLK VIRTIO_MMIO SECCOMP FUTEX EPOLL INOTIFY_USER FANOTIFY UNIX INET IPV6; do
  scripts/config --enable "$opt"
done
make olddefconfig >/dev/null
for opt in OVERLAY_FS VIRTIO_VSOCKETS EXT4_FS DEVTMPFS; do
  grep -q "^CONFIG_${opt}=y" .config || { echo "CONFIG_${opt} is not built in"; exit 1; }
done
if [ "$ARCH_NAME" = "aarch64" ]; then
  make -j"$(nproc)" Image >/dev/null
  cp arch/arm64/boot/Image /out/vmlinux
else
  make -j"$(nproc)" vmlinux >/dev/null
  cp vmlinux /out/vmlinux
fi
cp .config /out/vmlinux.config
INNER

echo "kernel written to $OUT/vmlinux"
