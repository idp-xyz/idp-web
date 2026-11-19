// Package pgtest gives the tests a scratch database on a real PostgreSQL. It is for tests only. Set IDP_TEST_PG_OWNER_URL
// to a superuser URL of a throwaway cluster (`make test-integration` starts one on a random loopback port and removes it
// again); without it the tests that need a database are skipped, as in idp-mcp and idp-host.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-web/internal/state"
)

// Scratch creates a database of its own, migrates it, creates the login role of the service and returns the owner's URL
// and the service's URL. The database is dropped when the test ends.
func Scratch(t testing.TB) (ownerURL, serviceURL string) {
	t.Helper()
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-integration)")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "idp_web_it_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	ownerURL = u.String()
	// Roles belong to the cluster, not to the database: the tests of several packages run at the same time and create the same
	// two roles, which PostgreSQL answers with "tuple concurrently updated". One at a time, then.
	unlock := RoleLock(t)
	defer unlock()
	if _, err := state.Migrate(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	if err := state.EnsureLogin(ctx, ownerURL, "idp_web_it_svc", "it-service-password"); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("idp_web_it_svc", "it-service-password")
	return ownerURL, u.String()
}

// Open opens a store of the scratch database as the service role, closed when the test ends. Each call is another
// "replica": its own pool, its own connections, the same database.
func Open(t testing.TB, serviceURL string, opt state.Options) *state.Store {
	t.Helper()
	s, err := state.Open(context.Background(), serviceURL, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// RoleLock takes the lock that serializes what creates roles in the cluster (Scratch does it for itself; a test that creates a
// role of its own does it too) and returns the function that releases it.
func RoleLock(t testing.TB) (unlock func()) {
	t.Helper()
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-integration)")
	}
	ctx := context.Background()
	lock, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock(72581)`); err != nil {
		t.Fatal(err)
	}
	return func() { lock.Close(ctx) } // closing the connection releases the lock
}
