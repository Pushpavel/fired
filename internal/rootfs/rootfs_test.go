package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func layer(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg, Uid: 1000, Gid: 1000})
		tw.Write([]byte(content))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	return buf.Bytes()
}

// TestBuildFromTarball builds an ext4 rootfs from a two-layer image with a
// whiteout and checks the result with debugfs.
func TestBuildFromTarball(t *testing.T) {
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("e2fsprogs not installed")
	}
	if os.Getuid() != 0 {
		t.Skip("needs root to preserve ownership")
	}
	dir := t.TempDir()
	l1 := layer(t, map[string]string{"bin/hello": "#!/bin/sh\necho hi\n", "etc/gone": "x"},
		&tar.Header{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "usr/bin/hello", Typeflag: tar.TypeSymlink, Linkname: "/bin/hello"})
	l2 := layer(t, map[string]string{"etc/.wh.gone": ""})
	img := empty.Image
	for _, b := range [][]byte{l1, l2} {
		b := b
		lay, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
		if err != nil {
			t.Fatal(err)
		}
		img, err = mutate.AppendLayers(img, lay)
		if err != nil {
			t.Fatal(err)
		}
	}
	cf, _ := img.ConfigFile()
	cf.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "IMG=1"}
	img, _ = mutate.ConfigFile(img, cf)
	tarPath := filepath.Join(dir, "img.tar")
	ref, _ := name.ParseReference("test/img:1")
	if err := tarball.WriteToFile(tarPath, ref, img); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agent")
	os.WriteFile(agent, []byte("fake-agent"), 0o755)
	out := filepath.Join(dir, "rootfs.ext4")
	res, err := Build(context.Background(), Options{Image: "tar:" + tarPath, AgentBinary: agent, Env: map[string]string{"EXTRA": "a b"}},
		filepath.Join(dir, "staging"), out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Env["IMG"] != "1" || res.Env["EXTRA"] != "a b" || res.Workdir != "/workspace" {
		t.Fatalf("result %+v", res)
	}
	ls := func(p string) string {
		b, _ := exec.Command("debugfs", "-R", "ls -l "+p, out).CombinedOutput()
		return string(b)
	}
	cat := func(p string) string {
		b, _ := exec.Command("debugfs", "-R", "cat "+p, out).Output()
		return string(b)
	}
	if !strings.Contains(ls("/bin"), "hello") || !strings.Contains(ls("/sbin"), "fired-agent") {
		t.Fatalf("missing files:\n%s\n%s", ls("/bin"), ls("/sbin"))
	}
	if strings.Contains(ls("/etc"), "gone") {
		t.Fatal("whiteout not applied")
	}
	for _, d := range []string{"/fired", "/workspace", "/proc"} {
		if strings.Contains(ls(d), "File not found") {
			t.Fatalf("%s missing", d)
		}
	}
	if env := cat("/etc/fired/env"); !strings.Contains(env, "IMG=1\n") || !strings.Contains(env, "EXTRA=a b\n") {
		t.Fatalf("env file %q", env)
	}
	if prof := cat("/etc/profile.d/fired.sh"); !strings.Contains(prof, "export EXTRA='a b'") {
		t.Fatalf("profile %q", prof)
	}
	if b, err := exec.Command("e2fsck", "-fn", out).CombinedOutput(); err != nil {
		t.Fatalf("fsck: %v\n%s", err, b)
	}
}
