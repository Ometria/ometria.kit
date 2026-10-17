package ecomm

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pgEnv is every variable pgconn's ParseConfig reads, set to a value that
// would change or break the config if it leaked through.
var pgEnv = map[string]string{
	"PGHOST":               "evil.example",
	"PGPORT":               "6543",
	"PGDATABASE":           "postgres",
	"PGUSER":               "postgres",
	"PGPASSWORD":           "from-env",
	"PGPASSFILE":           "", // set per test to a real passfile
	"PGAPPNAME":            "from-env",
	"PGCONNECT_TIMEOUT":    "not-a-number",
	"PGSSLMODE":            "verify-full",
	"PGSSLKEY":             "/nonexistent/postgresql.key",
	"PGSSLCERT":            "/nonexistent/postgresql.crt",
	"PGSSLSNI":             "0",
	"PGSSLROOTCERT":        "/nonexistent/root.crt",
	"PGSSLPASSWORD":        "from-env",
	"PGSSLNEGOTIATION":     "direct",
	"PGTARGETSESSIONATTRS": "read-write",
	"PGSERVICEFILE":        "/nonexistent/pg_service.conf",
	"PGTZ":                 "Asia/Tokyo",
	"PGOPTIONS":            "-c statement_timeout=5s",
	"PGMINPROTOCOLVERSION": "3.2",
	"PGMAXPROTOCOLVERSION": "3.2",
	"PGCHANNELBINDING":     "require",
	"PGREQUIREAUTH":        "gss",
}

// clearPGEnv unsets every PG* variable and points HOME at an empty
// directory, so the baseline does not depend on the developer's machine.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for k := range pgEnv {
		t.Setenv(k, "")
	}
	t.Setenv("PGSERVICE", "")
	t.Setenv("HOME", t.TempDir())
}

// configSnapshot is the comparable part of a pgxpool.Config.
type configSnapshot struct {
	Host, Database, User, Password string
	Port                           uint16
	TLS                            tlsSnapshot
	Fallbacks                      []string
	RuntimeParams                  map[string]string
	ConnectTimeout                 time.Duration
	HasValidateConnect             bool
	SSLNegotiation                 string
	KerberosSrvName, KerberosSpn   string
	MinProto, MaxProto             string
	ChannelBinding, RequireAuth    string
	ExecMode                       string
	StmtCache, DescCache           int
	MaxConns, MinConns             int32
	Idle, Lifetime, Health         time.Duration
}

type tlsSnapshot struct {
	Set                bool
	InsecureSkipVerify bool
	ServerName         string
	NextProtos         []string
	HasRootCAs         bool
	Certificates       int
}

func snapshot(t *testing.T, cfg Config, dbName string, readOnly bool) configSnapshot {
	t.Helper()
	pc, err := cfg.withDefaults().poolConfig(dbName, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	cc := pc.ConnConfig
	s := configSnapshot{
		Host: cc.Host, Port: cc.Port, Database: cc.Database, User: cc.User, Password: cc.Password,
		RuntimeParams:      cc.RuntimeParams,
		ConnectTimeout:     cc.ConnectTimeout,
		HasValidateConnect: cc.ValidateConnect != nil,
		SSLNegotiation:     cc.SSLNegotiation,
		KerberosSrvName:    cc.KerberosSrvName, KerberosSpn: cc.KerberosSpn,
		MinProto: cc.MinProtocolVersion, MaxProto: cc.MaxProtocolVersion,
		ChannelBinding: cc.ChannelBinding, RequireAuth: cc.RequireAuth,
		ExecMode:  cc.DefaultQueryExecMode.String(),
		StmtCache: cc.StatementCacheCapacity, DescCache: cc.DescriptionCacheCapacity,
		MaxConns: pc.MaxConns, MinConns: pc.MinConns,
		Idle: pc.MaxConnIdleTime, Lifetime: pc.MaxConnLifetime, Health: pc.HealthCheckPeriod,
	}
	if tc := cc.TLSConfig; tc != nil {
		s.TLS = tlsSnapshot{
			Set: true, InsecureSkipVerify: tc.InsecureSkipVerify, ServerName: tc.ServerName,
			NextProtos: tc.NextProtos, HasRootCAs: tc.RootCAs != nil, Certificates: len(tc.Certificates),
		}
	}
	for _, fb := range cc.Fallbacks {
		s.Fallbacks = append(s.Fallbacks, fb.Host+" tls="+map[bool]string{true: "on", false: "off"}[fb.TLSConfig != nil])
	}
	return s
}

func TestPoolConfigIgnoresPGEnvironment(t *testing.T) {
	// A $HOME with a .pgpass that matches anything, and garbage client
	// certificates where pgconn looks by default.
	writeHome := func(t *testing.T) string {
		home := t.TempDir()
		pgpass := filepath.Join(home, ".pgpass")
		if err := os.WriteFile(pgpass, []byte("*:*:*:*:from-pgpass\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, ".postgresql")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"root.crt", "postgresql.crt", "postgresql.key"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("not a pem file"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return home
	}

	for _, sslmode := range []string{"disable", "prefer", "require", "verify-full"} {
		for _, password := range []string{"secret", ""} {
			cfg := testConfig()
			cfg.SSLMode = sslmode
			cfg.Password = password
			cfg.ApplicationName = "push"
			for _, db := range []struct {
				name     string
				readOnly bool
			}{{"ometria_ecomm_0001", false}, {"ometria_ecomm_0001_ro", true}} {
				name := sslmode + "/password=" + password + "/" + db.name
				t.Run(name, func(t *testing.T) {
					clearPGEnv(t)
					want := snapshot(t, cfg, db.name, db.readOnly)
					if want.HasValidateConnect || want.SSLNegotiation != "postgres" {
						t.Errorf("baseline: ValidateConnect set=%v SSLNegotiation=%q", want.HasValidateConnect, want.SSLNegotiation)
					}

					home := writeHome(t)
					for k, v := range pgEnv {
						t.Run(k, func(t *testing.T) {
							clearPGEnv(t)
							if k == "PGPASSFILE" {
								v = filepath.Join(home, ".pgpass")
							}
							t.Setenv(k, v)
							if got := snapshot(t, cfg, db.name, db.readOnly); !reflect.DeepEqual(got, want) {
								t.Errorf("%s=%s changed the config:\n got %+v\nwant %+v", k, v, got, want)
							}
						})
					}
					t.Run("HOME", func(t *testing.T) {
						clearPGEnv(t)
						t.Setenv("HOME", home)
						if got := snapshot(t, cfg, db.name, db.readOnly); !reflect.DeepEqual(got, want) {
							t.Errorf("~/.pgpass or ~/.postgresql changed the config:\n got %+v\nwant %+v", got, want)
						}
					})
					t.Run("all", func(t *testing.T) {
						clearPGEnv(t)
						for k, v := range pgEnv {
							if k == "PGPASSFILE" {
								v = filepath.Join(home, ".pgpass")
							}
							t.Setenv(k, v)
						}
						t.Setenv("HOME", home)
						if got := snapshot(t, cfg, db.name, db.readOnly); !reflect.DeepEqual(got, want) {
							t.Errorf("PG* environment changed the config:\n got %+v\nwant %+v", got, want)
						}
					})
				})
			}
		}
	}
}

func TestPoolConfigRefusesPGSERVICE(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("PGSERVICE", "ecomm")
	_, err := testConfig().withDefaults().poolConfig("ometria_ecomm", false)
	if err == nil || !strings.Contains(err.Error(), "PGSERVICE") {
		t.Errorf("poolConfig with PGSERVICE set: err = %v, want a PGSERVICE error", err)
	}
	if _, err := NewResolver(testConfig(), staticLookup(nil)); err == nil || !strings.Contains(err.Error(), "PGSERVICE") {
		t.Errorf("NewResolver with PGSERVICE set: err = %v, want a PGSERVICE error", err)
	}
}
