// Package storetest gives tests their own Postgres database.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Env names the Postgres server tests use (`make test` starts one).
const Env = "FIRED_TEST_DATABASE_URL"

// URL creates an empty database for t on the server in $FIRED_TEST_DATABASE_URL
// and drops it when t ends. Tests are skipped when the variable is unset.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		t.Skipf("%s is not set; run `make test`, which starts a Postgres container", Env)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s: %v", Env, err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "fired_test_" + hex.EncodeToString(b)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	u.Path = "/" + name
	return u.String()
}
