package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Pushpavel/fired/internal/guest"
)

func statT(fi os.FileInfo) (*syscall.Stat_t, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return st, ok
}

// handleSync fixes up guest state after a boot or snapshot restore.
func (a *Agent) handleSync(w http.ResponseWriter, r *http.Request) {
	var req guest.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	var resp guest.SyncResponse
	var warnings []string
	if a.opt.RootPrefix == "" { // never touch the host clock in fake mode
		if req.UnixNanos > 0 {
			tv := unix.NsecToTimeval(req.UnixNanos)
			if err := unix.Settimeofday(&tv); err != nil {
				warnings = append(warnings, "settimeofday: "+err.Error())
			}
		}
		if len(req.Entropy) > 0 {
			if err := addEntropy(req.Entropy); err != nil {
				warnings = append(warnings, "entropy: "+err.Error())
			}
		}
		if req.GrowDisk {
			n, err := a.growDisk()
			if err != nil {
				warnings = append(warnings, "grow disk: "+err.Error())
			}
			resp.DiskBytes = n
		}
	}
	resp.Warning = strings.Join(warnings, "; ")
	writeJSON(w, http.StatusOK, resp)
}

// addEntropy credits host-provided randomness to the kernel pool so that
// clones restored from one snapshot diverge immediately.
func addEntropy(b []byte) error {
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	buf := make([]byte, 8+len(b))
	binary.NativeEndian.PutUint32(buf[0:], uint32(len(b)*8))
	binary.NativeEndian.PutUint32(buf[4:], uint32(len(b)))
	copy(buf[8:], b)
	const RNDADDENTROPY = 0x40085203
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), RNDADDENTROPY, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		// Fall back to mixing without crediting.
		_, werr := f.Write(b)
		if werr != nil {
			return fmt.Errorf("ioctl: %v, write: %v", errno, werr)
		}
	}
	return nil
}

// growDisk grows the writable ext4 filesystem online to fill /dev/vdb.
func (a *Agent) growDisk() (int64, error) {
	if a.opt.UpperFD < 0 {
		return 0, fmt.Errorf("no writable disk handle")
	}
	sectors, err := os.ReadFile("/sys/block/vdb/size")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(sectors)), 10, 64)
	if err != nil {
		return 0, err
	}
	devBytes := n * 512
	var st unix.Statfs_t
	if err := unix.Fstatfs(a.opt.UpperFD, &st); err != nil {
		return 0, err
	}
	bsize := int64(st.Bsize)
	if bsize <= 0 {
		return 0, fmt.Errorf("bad block size")
	}
	newBlocks := uint64(devBytes / bsize)
	if newBlocks <= st.Blocks {
		return devBytes, nil
	}
	const EXT4_IOC_RESIZE_FS = 0x40086610
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(a.opt.UpperFD), EXT4_IOC_RESIZE_FS, uintptr(unsafe.Pointer(&newBlocks)))
	if errno != 0 {
		return 0, errno
	}
	return devBytes, nil
}

// RunWithTimeout is a small helper used by init for best-effort steps.
func RunWithTimeout(d time.Duration, f func() error) error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		return fmt.Errorf("timed out after %s", d)
	}
}
