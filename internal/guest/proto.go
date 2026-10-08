// Package guest defines the protocol between the host daemon and the guest
// agent (cmd/fired-agent), which runs inside every sandbox VM.
//
// The agent serves HTTP/1.1 on vsock port AgentPort. Besides the JSON
// endpoints below, a CONNECT request for "127.0.0.1:<port>" opens a raw TCP
// tunnel to that port inside the guest; the daemon uses this for port
// previews (HTTP and WebSocket).
package guest

import "time"

const (
	// AgentPort is the vsock port the guest agent listens on.
	AgentPort = 1024
	// EgressPort is the vsock port on the host (CID 2) that receives
	// outbound proxy connections from the guest.
	EgressPort = 3128
	// HostCID is the well-known vsock CID of the host.
	HostCID = 2
	// GuestCID is the CID assigned to every VM (each VM has its own vsock
	// device, so CIDs need not be unique across VMs).
	GuestCID = 3
)

type Health struct {
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"started_at"`
	EgressAddr string    `json:"egress_addr,omitempty"`
	Overlay    bool      `json:"overlay"`
}

// SyncRequest is sent after every boot/restore: restoring a snapshot leaves
// the guest clock stale and the RNG state identical across clones.
type SyncRequest struct {
	UnixNanos int64  `json:"unix_nanos"`
	Entropy   []byte `json:"entropy,omitempty"`
	// GrowDisk asks the agent to grow the writable filesystem to the size of
	// its block device (after the host enlarged it).
	GrowDisk bool `json:"grow_disk,omitempty"`
}

type SyncResponse struct {
	DiskBytes int64  `json:"disk_bytes,omitempty"`
	Warning   string `json:"warning,omitempty"`
}
