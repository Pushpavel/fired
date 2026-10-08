package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClient talks to the Firecracker API over its unix socket.
type apiClient struct {
	http *http.Client
}

func newAPIClient(sock string) *apiClient {
	return &apiClient{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
		Timeout: 2 * time.Minute, // snapshot creation of large VMs can take a while
	}}
}

func (c *apiClient) do(ctx context.Context, method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var fe struct {
			FaultMessage string `json:"fault_message"`
		}
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &fe)
		msg := fe.FaultMessage
		if msg == "" {
			msg = string(b)
		}
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, msg)
	}
	return nil
}

// Firecracker API request bodies (subset).

type machineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMib int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type drivePatch struct {
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host"`
}

type vsockDev struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type action struct {
	ActionType string `json:"action_type"`
}

type vmState struct {
	State string `json:"state"`
}

type snapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type snapshotLoad struct {
	SnapshotPath string     `json:"snapshot_path"`
	MemBackend   memBackend `json:"mem_backend"`
	ResumeVM     bool       `json:"resume_vm"`
}
