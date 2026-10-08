package agent

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Guest filesystem layout prepared by the template builder:
//
//	/dev/vda  read-only root filesystem (shared by all sandboxes of a template)
//	/dev/vdb  per-sandbox writable ext4 disk
//
// Init mounts an overlay (lower = vda, upper = vdb) and pivots into it, so
// the whole filesystem is writable while the template stays immutable.
const (
	firedDir   = "/fired"
	lowerDir   = "/fired/lower"
	upperMnt   = "/fired/upper"
	newRootDir = "/fired/newroot"
	oldRootRel = "fired/oldroot"
	UpperDev   = "/dev/vdb"
)

// InitResult is passed from init to the serve child.
type InitResult struct {
	Overlay bool
	UpperFD *os.File
}

func mount(src, target, fstype string, flags uintptr, data string) error {
	if err := unix.Mount(src, target, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s (%s): %w", src, target, fstype, err)
	}
	return nil
}

// RunInit is the PID 1 entrypoint inside the VM. It never returns.
func RunInit(self string, serveArgs []string) {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("component", "fired-init")
	res, err := setupFilesystem(log)
	if err != nil {
		log.Error("filesystem setup failed; continuing on the read-only root", "err", err)
	}
	setupNetwork(log)
	_ = unix.Sethostname([]byte("sandbox"))

	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)

	var child *exec.Cmd
	start := func() {
		args := append([]string{}, serveArgs...)
		cmd := exec.Command(self, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
		if res.Overlay {
			cmd.Env = append(cmd.Env, "FIRED_OVERLAY=1")
		}
		if res.UpperFD != nil {
			cmd.ExtraFiles = []*os.File{res.UpperFD} // fd 3
			cmd.Env = append(cmd.Env, "FIRED_UPPER_FD=3")
		}
		if err := cmd.Start(); err != nil {
			log.Error("start agent", "err", err)
			return
		}
		child = cmd
	}
	start()

	go func() {
		<-term
		if child != nil && child.Process != nil {
			_ = child.Process.Signal(syscall.SIGTERM)
		}
		time.Sleep(2 * time.Second)
		unix.Sync()
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	}()

	// Reap everything; restart the agent if it dies.
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if err == unix.ECHILD {
				time.Sleep(time.Second)
				if child == nil {
					start()
				}
				continue
			}
			continue
		}
		if child != nil && child.Process != nil && pid == child.Process.Pid {
			log.Error("agent exited, restarting", "status", ws.ExitStatus())
			child = nil
			time.Sleep(500 * time.Millisecond)
			start()
		}
	}
}

func setupFilesystem(log *slog.Logger) (InitResult, error) {
	var res InitResult
	_ = os.MkdirAll("/proc", 0o555)
	_ = mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	_ = mount("sysfs", "/sys", "sysfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	_ = mount("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755")

	// The root (vda) is read-only. Find a writable upper layer.
	upperOK := false
	if _, err := os.Stat(UpperDev); err == nil {
		if err := mount(UpperDev, upperMnt, "ext4", unix.MS_NOATIME, ""); err != nil {
			log.Error("mount writable disk", "err", err)
		} else {
			upperOK = true
		}
	}
	if !upperOK {
		if err := mount("tmpfs", upperMnt, "tmpfs", 0, "mode=0755"); err != nil {
			return res, err
		}
		log.Warn("no writable disk; changes are kept in memory only")
	}
	if f, err := os.Open(upperMnt); err == nil {
		res.UpperFD = f
	}
	_ = os.MkdirAll(upperMnt+"/data", 0o755)
	_ = os.MkdirAll(upperMnt+"/work", 0o755)

	if err := mount("/", lowerDir, "", unix.MS_BIND, ""); err != nil {
		return res, err
	}
	opts := "lowerdir=" + lowerDir + ",upperdir=" + upperMnt + "/data,workdir=" + upperMnt + "/work"
	if err := mount("overlay", newRootDir, "overlay", 0, opts); err != nil {
		// Fallback: keep the read-only root and bind writable directories.
		log.Error("overlayfs unavailable; only some directories will be writable (enable CONFIG_OVERLAY_FS in the guest kernel)", "err", err)
		for _, d := range []string{"/workspace", "/root", "/home", "/tmp", "/var/tmp"} {
			src := filepath.Join(upperMnt, "data", d)
			_ = os.MkdirAll(src, 0o755)
			_ = mount(src, d, "", unix.MS_BIND, "")
		}
		_ = os.Chmod(filepath.Join(upperMnt, "data", "tmp"), 0o777|os.ModeSticky)
		mountRuntime()
		return res, nil
	}
	res.Overlay = true
	for _, m := range []string{"/proc", "/sys", "/dev"} {
		if err := mount(m, newRootDir+m, "", unix.MS_MOVE, ""); err != nil {
			return res, err
		}
	}
	if err := unix.PivotRoot(newRootDir, filepath.Join(newRootDir, oldRootRel)); err != nil {
		return res, fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return res, err
	}
	if err := unix.Unmount("/"+oldRootRel, unix.MNT_DETACH); err != nil {
		log.Warn("detach old root", "err", err)
	}
	mountRuntime()
	return res, nil
}

func mountRuntime() {
	_ = os.MkdirAll("/dev/pts", 0o755)
	_ = mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5")
	if _, err := os.Lstat("/dev/ptmx"); err != nil {
		_ = os.Symlink("pts/ptmx", "/dev/ptmx")
	}
	_ = os.MkdirAll("/dev/shm", 0o777|os.ModeSticky)
	_ = mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777")
	_ = os.MkdirAll("/run", 0o755)
	_ = mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755")
	_ = os.MkdirAll("/sys/fs/cgroup", 0o755)
	_ = mount("cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	_ = os.Chmod("/tmp", 0o777|os.ModeSticky)
}

// setupNetwork brings up the loopback interface. Sandboxes have no other
// network interface; outbound traffic uses the vsock egress proxy.
func setupNetwork(log *slog.Logger) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		log.Error("socket", "err", err)
		return
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		log.Error("get lo flags", "err", err)
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		log.Error("bring up lo", "err", err)
	}
}
