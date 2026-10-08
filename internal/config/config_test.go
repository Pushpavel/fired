package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`
listen: ":9000"
driver: fake
defaults:
  vcpus: 4
  memory_mb: 1024
  disk_mb: 4096
  ttl:
    idle_suspend: 5m
    archive_after: "-1s"
egress:
  enabled: true
  global_allow: ["registry.npmjs.org"]
`), 0o644)
	t.Setenv("FIRED_API_TOKEN", "tok")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.Driver != "fake" || c.Defaults.VCPUs != 4 || c.Auth.Token != "tok" {
		t.Fatalf("%+v", c)
	}
	if c.Defaults.TTL.IdleSuspend.D() != 5*time.Minute || c.Defaults.TTL.ArchiveAfter.D() != -time.Second {
		t.Fatalf("ttl %+v", c.Defaults.TTL)
	}
	if c.Defaults.TTL.DeleteAfter.D() != 7*24*time.Hour {
		t.Fatal("unset fields should keep defaults")
	}
	os.WriteFile(p, []byte("bogus_key: 1\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown keys should be rejected")
	}
}
