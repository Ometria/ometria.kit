package ecomm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The integration test runs Postgres 14 behind PgBouncer in transaction mode,
// configured like ometria.platform.pgbouncer's prod values. Postgres is
// started with a non-UTC TimeZone and non-ISO DateStyle so the test proves
// the pinned startup parameters take effect rather than matching by luck.
//
// Images can be overridden with ECOMM_IT_POSTGRES_IMAGE and
// ECOMM_IT_PGBOUNCER_IMAGE. The default PgBouncer image is pinned to the
// tag for the release production runs (1.25.1), though edoburu rebuilds tags
// and v1.25.1-p0 currently ships the 1.25.2 binary; the test logs the version
// it got. Any image with pgbouncer on its PATH works.

const (
	itUser     = "ometria"
	itPassword = "Ometria123"

	defaultPostgresImage  = "postgres:14"
	defaultPgBouncerImage = "edoburu/pgbouncer:v1.25.1-p0"
)

// pgbouncerINI mirrors helm/ew1-prod/values-readonly.yaml.gotmpl, except
// default_pool_size = 1 so every client shares one server connection, which
// is the worst case for leaked session state. Each "_ro" alias points at its
// own backend database, standing in for the replica, so the routing test can
// tell a read-only pool from a read-write one.
const pgbouncerINI = `
[databases]
ometria_ecomm = host=pg port=5432 dbname=ometria_ecomm_0000
ometria_ecomm_ro = host=pg port=5432 dbname=ometria_ecomm_replica
ometria_ecomm_0001 = host=pg port=5432 dbname=ometria_ecomm_0001
ometria_ecomm_0001_ro = host=pg port=5432 dbname=ometria_ecomm_0001_replica

[pgbouncer]
listen_addr = *
listen_port = 5432
auth_type = md5
auth_file = /etc/pgbouncer-it/userlist.txt
pool_mode = transaction
server_reset_query = DISCARD ALL
server_check_query = select 1
server_tls_sslmode = allow
ignore_startup_parameters = extra_float_digits
max_prepared_statements = 200
default_pool_size = 1
reserve_pool_size = 0
max_client_conn = 100
`

const initSQL = `
CREATE DATABASE ometria_ecomm_0000;
CREATE DATABASE ometria_ecomm_0001;
CREATE DATABASE ometria_ecomm_replica;
CREATE DATABASE ometria_ecomm_0001_replica;
`

func TestIntegrationPgBouncer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	host, port := startStack(t)

	lookup := staticLookup(map[int]string{1: "0001", 2: "0000"})
	newResolver := func(t *testing.T, tz, ds string) *Resolver {
		t.Helper()
		cfg := Config{
			Host: host, Port: port, User: itUser, Password: itPassword, SSLMode: "disable",
			TimeZone: tz, DateStyle: ds, ApplicationName: "ecomm-it",
		}
		r, err := NewResolver(cfg, lookup)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	utc := newResolver(t, DefaultTimeZone, DefaultDateStyle)
	ro, err := utc.Pool(ctx, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("routes to shard database", func(t *testing.T) {
		for _, tc := range []struct {
			account  int
			readOnly bool
			want     string
		}{
			{1, true, "ometria_ecomm_0001_replica"},
			{1, false, "ometria_ecomm_0001"},
			{2, true, "ometria_ecomm_replica"},
			{2, false, "ometria_ecomm_0000"},
		} {
			p, err := utc.Pool(ctx, tc.account, tc.readOnly)
			if err != nil {
				t.Fatal(err)
			}
			var db string
			if err := p.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
				t.Fatalf("account %d ro=%v: %v", tc.account, tc.readOnly, err)
			}
			if db != tc.want {
				t.Errorf("account %d ro=%v: current_database() = %s, want %s", tc.account, tc.readOnly, db, tc.want)
			}
		}
	})

	t.Run("timestamptz text matches PHP", func(t *testing.T) {
		var got, tz, ds string
		err := ro.QueryRow(ctx,
			"SELECT '2026-10-17 12:00:00.12+00'::timestamptz::text, current_setting('TimeZone'), current_setting('DateStyle')",
		).Scan(&got, &tz, &ds)
		if err != nil {
			t.Fatal(err)
		}
		if got != "2026-10-17 12:00:00.12+00" {
			t.Errorf("timestamptz::text = %q, want %q (TimeZone=%s DateStyle=%s)", got, "2026-10-17 12:00:00.12+00", tz, ds)
		}
		if tz != "UTC" || ds != "ISO, MDY" {
			t.Errorf("TimeZone/DateStyle = %s/%s, want UTC/ISO, MDY", tz, ds)
		}
		// Golden shapes from PHP captures: trailing zeros trimmed, no
		// fraction on whole seconds.
		for in, want := range map[string]string{
			"2026-10-17 10:00:01.500+00":    "2026-10-17 10:00:01.5+00",
			"2026-10-17 10:00:00+00":        "2026-10-17 10:00:00+00",
			"2026-10-17 11:54:27.752599+01": "2026-10-17 10:54:27.752599+00",
		} {
			var got string
			if err := ro.QueryRow(ctx, "SELECT $1::timestamptz::text", in).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("%s::timestamptz::text = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("simple protocol through transaction pooling", func(t *testing.T) {
		// More client connections than PgBouncer's single server connection,
		// with parameters and explicit transactions.
		var wg sync.WaitGroup
		for i := range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var sum int
				if err := ro.QueryRow(ctx, "SELECT $1::int + $2::int", i, 1).Scan(&sum); err != nil {
					t.Error(err)
					return
				}
				if sum != i+1 {
					t.Errorf("sum = %d, want %d", sum, i+1)
				}
				tx, err := ro.Begin(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				defer tx.Rollback(ctx)
				var s string
				if err := tx.QueryRow(ctx, "SELECT $1::text", fmt.Sprint(i)).Scan(&s); err != nil {
					t.Error(err)
					return
				}
				if err := tx.Commit(ctx); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()

		var n int
		if err := ro.QueryRow(ctx, "SELECT count(*) FROM pg_prepared_statements").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("pg_prepared_statements has %d rows, want 0", n)
		}

		// Control: pgx's default extended protocol does leave statements on
		// the shared server connection, so the check above can fail.
		cc, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=ometria_ecomm_0001_ro user=%s password=%s sslmode=disable",
			host, port, itUser, itPassword))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pgx.ConnectConfig(ctx, cc)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "SELECT $1::int", 1); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_prepared_statements").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Error("control: extended protocol left no prepared statement, so the check above proves nothing")
		}
	})

	t.Run("PgBouncer keeps TimeZone per client on a shared server connection", func(t *testing.T) {
		london := newResolver(t, "Europe/London", "ISO, DMY")
		lp, err := london.Pool(ctx, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		const q = "SELECT '2026-10-17 12:00:00.12+00'::timestamptz::text || ' ' || current_setting('DateStyle')"
		for i := range 3 {
			var a, b string
			if err := lp.QueryRow(ctx, q).Scan(&a); err != nil {
				t.Fatal(err)
			}
			if err := ro.QueryRow(ctx, q).Scan(&b); err != nil {
				t.Fatal(err)
			}
			if a != "2026-10-17 13:00:00.12+01 ISO, DMY" || b != "2026-10-17 12:00:00.12+00 ISO, MDY" {
				t.Errorf("round %d: london=%q utc=%q", i, a, b)
			}
		}
	})

	t.Run("unpinned connection gets the server default", func(t *testing.T) {
		// Without the startup parameters the role/server default applies,
		// which is why Config makes them required.
		cc, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=ometria_ecomm_0001_ro user=%s password=%s sslmode=disable default_query_exec_mode=simple_protocol",
			host, port, itUser, itPassword))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pgx.ConnectConfig(ctx, cc)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var got string
		if err := conn.QueryRow(ctx, "SELECT '2026-10-17 12:00:00.12+00'::timestamptz::text").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == "2026-10-17 12:00:00.12+00" {
			t.Errorf("unpinned connection printed %q; the server default should differ in this test", got)
		}
	})

	t.Run("statement_timeout startup parameter is rejected", func(t *testing.T) {
		cc, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=ometria_ecomm_0001_ro user=%s password=%s sslmode=disable",
			host, port, itUser, itPassword))
		if err != nil {
			t.Fatal(err)
		}
		cc.RuntimeParams["statement_timeout"] = "5000"
		conn, err := pgx.ConnectConfig(ctx, cc)
		if err == nil {
			conn.Close(ctx)
			t.Fatal("PgBouncer accepted statement_timeout as a startup parameter")
		}
		if !strings.Contains(err.Error(), "unsupported startup parameter") {
			t.Errorf("err = %v, want unsupported startup parameter", err)
		}
	})

	t.Run("context deadline cancels a query", func(t *testing.T) {
		qctx, qcancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer qcancel()
		start := time.Now()
		_, err := ro.Exec(qctx, "SELECT pg_sleep(10)")
		if err == nil {
			t.Fatal("pg_sleep(10) finished despite a 200ms deadline")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("query returned after %v", d)
		}
		var one int
		if err := ro.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("pool unusable after a cancelled query: %v", err)
		}
	})

	t.Run("stats", func(t *testing.T) {
		stats := utc.Stats()
		if len(stats) != 4 {
			t.Fatalf("Stats() has %d pools, want 4", len(stats))
		}
		for _, s := range stats {
			if s.Stat.TotalConns() > DefaultMaxConns {
				t.Errorf("%s: %d conns exceeds MaxConns", s.Database, s.Stat.TotalConns())
			}
		}
	})
}

// startStack starts Postgres 14 and PgBouncer on a private Docker network
// and returns PgBouncer's published address. It skips the test when Docker
// is not available.
func startStack(t *testing.T) (string, uint16) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found; skipping integration test")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker unavailable (%v: %s); skipping integration test", err, strings.TrimSpace(string(out)))
	}
	pgImage := envOr("ECOMM_IT_POSTGRES_IMAGE", defaultPostgresImage)
	bouncerImage := envOr("ECOMM_IT_PGBOUNCER_IMAGE", defaultPgBouncerImage)

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id := "ecomm-it-" + hex.EncodeToString(b)
	network, pgName, bouncerName := id, id+"-pg", id+"-pgbouncer"

	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pgbouncer.ini", pgbouncerINI)
	write("userlist.txt", fmt.Sprintf("%q %q\n", itUser, itPassword))
	write("init.sql", initSQL)
	// TempDir is 0700; the containers run as non-root users.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	docker(t, "network", "create", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })

	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pgName, bouncerName).Run() })
	docker(t, "run", "-d", "--name", pgName, "--network", network, "--network-alias", "pg",
		"-e", "POSTGRES_USER="+itUser, "-e", "POSTGRES_PASSWORD="+itPassword,
		"-v", filepath.Join(dir, "init.sql")+":/docker-entrypoint-initdb.d/init.sql:ro",
		pgImage, "-c", "TimeZone=Europe/London", "-c", "DateStyle=SQL, DMY")
	docker(t, "run", "-d", "--name", bouncerName, "--network", network,
		"-p", "127.0.0.1::5432",
		"-v", dir+":/etc/pgbouncer-it:ro",
		"--entrypoint", "pgbouncer", // resolved on the image's PATH
		bouncerImage, "/etc/pgbouncer-it/pgbouncer.ini")

	if v, err := exec.Command("docker", "exec", bouncerName, "pgbouncer", "--version").CombinedOutput(); err == nil {
		t.Logf("%s: %s", bouncerImage, strings.SplitN(strings.TrimSpace(string(v)), "\n", 2)[0])
	}

	out := docker(t, "port", bouncerName, "5432/tcp")
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(strings.Split(out, "\n")[0]))
	if err != nil {
		t.Fatalf("parse docker port output %q: %v", out, err)
	}
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	host, port := "127.0.0.1", uint16(p)

	// Wait until Postgres has finished init and PgBouncer can reach it.
	dsn := fmt.Sprintf("host=%s port=%d dbname=ometria_ecomm_0001 user=%s password=%s sslmode=disable default_query_exec_mode=simple_protocol connect_timeout=2",
		host, port, itUser, itPassword)
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			err = conn.Ping(ctx)
			conn.Close(ctx)
		}
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "50", bouncerName).CombinedOutput()
			t.Fatalf("stack not ready: %v\npgbouncer logs:\n%s", err, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return host, port
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
