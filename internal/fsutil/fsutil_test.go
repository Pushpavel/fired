package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpackSparse(t *testing.T) {
	src := t.TempDir()
	// 64 MiB sparse file with data at two offsets.
	f, err := os.Create(filepath.Join(src, "disk.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.WriteAt(bytes.Repeat([]byte("A"), 8192), 4096)
	f.WriteAt([]byte("tail"), 64<<20-4)
	f.Close()
	os.MkdirAll(filepath.Join(src, "fs/sub"), 0o750)
	os.WriteFile(filepath.Join(src, "fs/sub/file"), []byte("hello"), 0o640)
	os.Symlink("sub/file", filepath.Join(src, "fs/link"))
	os.WriteFile(filepath.Join(src, "skipme"), []byte("x"), 0o644)

	ar := filepath.Join(t.TempDir(), "a.zst")
	if err := Pack(src, ar, func(rel string, _ os.DirEntry) bool { return rel == "skipme" }); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(ar)
	if st.Size() > 1<<20 {
		t.Fatalf("archive too large for sparse input: %d", st.Size())
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := Unpack(ar, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "disk.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64<<20 || got[4096] != 'A' || got[4096+8191] != 'A' || string(got[len(got)-4:]) != "tail" || got[0] != 0 {
		t.Fatal("sparse file content mismatch")
	}
	if alloc := AllocatedBytes(filepath.Join(dst, "disk.ext4")); alloc > 8<<20 {
		t.Fatalf("holes not preserved: %d bytes allocated", alloc)
	}
	b, _ := os.ReadFile(filepath.Join(dst, "fs/link"))
	if string(b) != "hello" {
		t.Fatalf("symlink: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "fs/sub/file")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(dst, "skipme")); !os.IsNotExist(err) {
		t.Fatal("skipped file was archived")
	}
}

func TestSparseCopy(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "a")
	f, _ := os.Create(src)
	f.Truncate(32 << 20)
	f.WriteAt([]byte("x"), 16<<20)
	f.Close()
	dst := filepath.Join(d, "b")
	if err := SparseCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if len(b) != 32<<20 || b[16<<20] != 'x' {
		t.Fatal("content mismatch")
	}
	if AllocatedBytes(dst) > 4<<20 {
		t.Fatal("copy is not sparse")
	}
}
