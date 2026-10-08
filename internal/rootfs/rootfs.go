// Package rootfs converts an OCI image into the read-only ext4 root
// filesystem of a Firecracker template.
package rootfs

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"golang.org/x/sys/unix"
)

type Options struct {
	// Image is a registry reference ("node:22", "registry.local:5000/agent:1")
	// or "tar:/path/image.tar" (output of `docker save`).
	Image string
	// InsecureRegistries lists registry hosts reachable over plain HTTP.
	InsecureRegistries []string
	// AgentBinary is copied to /sbin/fired-agent.
	AgentBinary string
	// SizeMB of the ext4 image; 0 = content size + 30% + 256 MiB.
	SizeMB  int
	Env     map[string]string
	Workdir string
	Log     io.Writer
}

// Result describes the built image.
type Result struct {
	SizeMB  int
	Workdir string
	Env     map[string]string
}

// Build writes outPath (ext4) from the image. stagingDir must be on a
// filesystem with room for the unpacked image; it is removed afterwards.
func Build(ctx context.Context, o Options, stagingDir, outPath string) (*Result, error) {
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	img, err := loadImage(ctx, o)
	if err != nil {
		return nil, err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	_ = os.RemoveAll(stagingDir)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagingDir)

	logf("extracting %s", o.Image)
	rc := mutate.Extract(img)
	n, err := extract(rc, stagingDir)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	logf("extracted %d entries", n)

	env := map[string]string{}
	for _, kv := range cfg.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for k, v := range o.Env {
		env[k] = v
	}
	workdir := o.Workdir
	if workdir == "" {
		workdir = "/workspace"
	}
	if err := inject(stagingDir, o.AgentBinary, env, workdir); err != nil {
		return nil, fmt.Errorf("inject agent: %w", err)
	}

	sizeMB := o.SizeMB
	if sizeMB <= 0 {
		used, err := duBytes(stagingDir)
		if err != nil {
			return nil, err
		}
		sizeMB = int(used*13/10>>20) + 256
	}
	logf("creating %d MiB ext4 image", sizeMB)
	_ = os.Remove(outPath)
	tmp := outPath + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	// No journal: the image is mounted read-only by every sandbox.
	cmd := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", "-L", "rootfs", "-O", "^has_journal",
		"-E", "root_owner=0:0", "-d", stagingDir, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("mkfs.ext4 -d: %v: %s", err, out)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return nil, err
	}
	return &Result{SizeMB: sizeMB, Workdir: workdir, Env: env}, nil
}

func loadImage(ctx context.Context, o Options) (v1.Image, error) {
	if p, ok := strings.CutPrefix(o.Image, "tar:"); ok {
		return tarball.ImageFromPath(p, nil)
	}
	var opts []name.Option
	ref0, err := name.ParseReference(o.Image)
	if err != nil {
		return nil, err
	}
	for _, h := range o.InsecureRegistries {
		if ref0.Context().RegistryStr() == h {
			opts = append(opts, name.Insecure)
		}
	}
	ref, err := name.ParseReference(o.Image, opts...)
	if err != nil {
		return nil, err
	}
	return remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	)
}

func safePath(root, name string) (string, error) {
	p := filepath.Join(root, filepath.Clean("/"+name))
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("entry %q escapes root", name)
	}
	return p, nil
}

func extract(r io.Reader, root string) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	type dirMeta struct {
		path string
		mode os.FileMode
		mt   time.Time
	}
	var dirs []dirMeta
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		target, err := safePath(root, h.Name)
		if err != nil {
			return n, err
		}
		if target == root {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return n, err
		}
		mode := os.FileMode(h.Mode & 0o777)
		if h.Mode&0o4000 != 0 {
			mode |= os.ModeSetuid
		}
		if h.Mode&0o2000 != 0 {
			mode |= os.ModeSetgid
		}
		if h.Mode&0o1000 != 0 {
			mode |= os.ModeSticky
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if st, err := os.Lstat(target); err == nil && !st.IsDir() {
				_ = os.RemoveAll(target)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
			dirs = append(dirs, dirMeta{target, mode, h.ModTime})
		case tar.TypeReg:
			_ = os.RemoveAll(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return n, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return n, err
			}
			f.Close()
		case tar.TypeSymlink:
			_ = os.RemoveAll(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return n, err
			}
		case tar.TypeLink:
			src, err := safePath(root, h.Linkname)
			if err != nil {
				return n, err
			}
			_ = os.RemoveAll(target)
			if err := os.Link(src, target); err != nil {
				return n, err
			}
			continue // shares the source's metadata
		case tar.TypeFifo:
			_ = os.RemoveAll(target)
			if err := unix.Mkfifo(target, uint32(h.Mode&0o777)); err != nil {
				return n, err
			}
		default:
			continue // device nodes come from devtmpfs at boot
		}
		_ = os.Lchown(target, h.Uid, h.Gid)
		if h.Typeflag != tar.TypeSymlink {
			if h.Typeflag != tar.TypeDir {
				_ = os.Chmod(target, mode)
			}
			for k, v := range h.PAXRecords {
				if attr, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok {
					_ = unix.Lsetxattr(target, attr, []byte(v), 0)
				}
			}
			if h.Typeflag != tar.TypeDir {
				_ = os.Chtimes(target, h.ModTime, h.ModTime)
			}
		}
		n++
	}
	// Apply directory modes last (a read-only dir would block its children).
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		_ = os.Chmod(d.path, d.mode)
		_ = os.Chtimes(d.path, d.mt, d.mt)
	}
	return n, nil
}

func inject(root, agentBin string, env map[string]string, workdir string) error {
	for _, d := range []string{"fired/lower", "fired/upper", "fired/newroot", "fired/oldroot",
		"proc", "sys", "dev", "run", "tmp", "etc/fired", "sbin", "root", strings.TrimPrefix(workdir, "/")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}
	_ = os.Chmod(filepath.Join(root, "tmp"), 0o777|os.ModeSticky)
	in, err := os.Open(agentBin)
	if err != nil {
		return err
	}
	defer in.Close()
	agentPath := filepath.Join(root, "sbin", "fired-agent")
	_ = os.Remove(agentPath)
	out, err := os.OpenFile(agentPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		if strings.ContainsAny(env[k], "\n") {
			continue
		}
		sb.WriteString(k + "=" + env[k] + "\n")
	}
	if err := os.WriteFile(filepath.Join(root, "etc/fired/env"), []byte(sb.String()), 0o644); err != nil {
		return err
	}
	meta, _ := json.Marshal(map[string]string{"workdir": workdir})
	if err := os.WriteFile(filepath.Join(root, "etc/fired/template.json"), meta, 0o644); err != nil {
		return err
	}
	// No DNS inside the VM: tools resolve through the egress proxy.
	resolv := filepath.Join(root, "etc/resolv.conf")
	_ = os.Remove(resolv)
	_ = os.WriteFile(resolv, []byte("# sandbox: no direct network; use $HTTPS_PROXY\n"), 0o644)
	hosts := filepath.Join(root, "etc/hosts")
	if b, err := os.ReadFile(hosts); err != nil || !strings.Contains(string(b), "localhost") {
		_ = os.Remove(hosts)
		_ = os.WriteFile(hosts, []byte("127.0.0.1\tlocalhost sandbox\n::1\tlocalhost ip6-localhost\n"), 0o644)
	}
	// Login shells (which reset PATH via /etc/profile) get the template
	// environment back.
	var prof strings.Builder
	for _, k := range keys {
		if strings.ContainsAny(env[k], "\n") || !validEnvName(k) {
			continue
		}
		prof.WriteString("export " + k + "='" + strings.ReplaceAll(env[k], "'", `'\''`) + "'\n")
	}
	_ = os.MkdirAll(filepath.Join(root, "etc/profile.d"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc/profile.d/fired.sh"), []byte(prof.String()), 0o644)
	return nil
}

func validEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func duBytes(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		total += fi.Size() + 4096
		return nil
	})
	return total, err
}
