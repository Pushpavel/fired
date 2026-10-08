package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupRoot = "/sys/fs/cgroup"

// setupCgroupV2 prepares <cgroupRoot>/<parent> so the jailer can create a
// child cgroup per VM with cpu and memory limits.
//
// Inside a container with a private cgroup namespace the daemon sits in the
// namespace root, and cgroup v2 forbids enabling controllers for children of
// a cgroup that has processes ("no internal processes"). So first move all
// processes of the root into a leaf "daemon" cgroup.
func setupCgroupV2(parent string) error {
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return fmt.Errorf("cgroup v2 not mounted at %s", cgroupRoot)
	}
	ctrls, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return err
	}
	for _, c := range []string{"cpu", "memory"} {
		if !strings.Contains(" "+string(ctrls)+" ", " "+c+" ") && !strings.Contains(string(ctrls), c) {
			return fmt.Errorf("cgroup controller %q not available (have %q)", c, strings.TrimSpace(string(ctrls)))
		}
	}
	enable := func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644)
	}
	if err := enable(cgroupRoot); err != nil {
		// Probably blocked by processes in the root: evacuate them.
		leaf := filepath.Join(cgroupRoot, "daemon")
		if err := os.MkdirAll(leaf, 0o755); err != nil {
			return err
		}
		procs, _ := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.procs"))
		for _, p := range strings.Fields(string(procs)) {
			if _, err := strconv.Atoi(p); err != nil {
				continue
			}
			_ = os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(p), 0o644)
		}
		if err := enable(cgroupRoot); err != nil {
			return fmt.Errorf("enable controllers at %s: %w", cgroupRoot, err)
		}
	}
	pdir := filepath.Join(cgroupRoot, parent)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		return err
	}
	if err := enable(pdir); err != nil {
		return fmt.Errorf("enable controllers at %s: %w", pdir, err)
	}
	return nil
}
