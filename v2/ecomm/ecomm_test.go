package ecomm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatabaseName(t *testing.T) {
	tests := []struct {
		shard    string
		readOnly bool
		want     string
	}{
		{"0000", false, "ometria_ecomm"},
		{"0000", true, "ometria_ecomm_ro"},
		{"0001", false, "ometria_ecomm_0001"},
		{"0001", true, "ometria_ecomm_0001_ro"},
		{"0042", true, "ometria_ecomm_0042_ro"},
		{"00000", false, "ometria_ecomm_00000"}, // only the exact "0000" is special
	}
	for _, tt := range tests {
		got, err := DatabaseName(tt.shard, tt.readOnly)
		if err != nil {
			t.Errorf("DatabaseName(%q, %v): %v", tt.shard, tt.readOnly, err)
			continue
		}
		if got != tt.want {
			t.Errorf("DatabaseName(%q, %v) = %q, want %q", tt.shard, tt.readOnly, got, tt.want)
		}
	}
}

func TestDatabaseNameRejectsBadShards(t *testing.T) {
	if _, err := DatabaseName("", false); !errors.Is(err, ErrNoShard) {
		t.Errorf("empty shard: err = %v, want ErrNoShard", err)
	}
	for _, s := range []string{"00 01", "0001'", "a/b", "shard-3", "0001;"} {
		if _, err := DatabaseName(s, false); err == nil {
			t.Errorf("DatabaseName(%q) succeeded, want error", s)
		}
	}
}

func testConfig() Config {
	return Config{
		Host:      "127.0.0.1",
		Port:      1, // never dialled by these tests
		User:      "ometria",
		Password:  `p'w\d`,
		SSLMode:   "disable",
		TimeZone:  DefaultTimeZone,
		DateStyle: DefaultDateStyle,
	}
}

func staticLookup(shards map[int]string) ShardLookup {
	return ShardLookupFunc(func(_ context.Context, id int) (Shard, error) {
		s, ok := shards[id]
		if !ok {
			return Shard{}, errors.New("unknown account")
		}
		return Shard{DbShard: s}, nil
	})
}

func TestConfigValidation(t *testing.T) {
	lookup := staticLookup(nil)
	for name, mutate := range map[string]func(*Config){
		"no host":      func(c *Config) { c.Host = "" },
		"no port":      func(c *Config) { c.Port = 0 },
		"no user":      func(c *Config) { c.User = "" },
		"bad sslmode":  func(c *Config) { c.SSLMode = "" },
		"no timezone":  func(c *Config) { c.TimeZone = "" },
		"no datestyle": func(c *Config) { c.DateStyle = "" },
		"min > max":    func(c *Config) { c.MaxConns, c.MinConns = 2, 3 },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if _, err := NewResolver(cfg, lookup); err == nil {
			t.Errorf("%s: NewResolver succeeded, want error", name)
		}
	}
	if _, err := NewResolver(testConfig(), nil); err == nil {
		t.Error("nil lookup: NewResolver succeeded, want error")
	}
}

func TestPoolConfig(t *testing.T) {
	cfg := testConfig()
	cfg.ReadOnlyHost = "ro.example"
	cfg.ApplicationName = "push"
	cfg = cfg.withDefaults()

	pc, err := cfg.poolConfig("ometria_ecomm_0001_ro", true)
	if err != nil {
		t.Fatal(err)
	}
	cc := pc.ConnConfig
	if cc.Host != "ro.example" || cc.Port != 1 || cc.Database != "ometria_ecomm_0001_ro" ||
		cc.User != "ometria" || cc.Password != `p'w\d` {
		t.Errorf("conn target = %s:%d/%s user=%s password=%q", cc.Host, cc.Port, cc.Database, cc.User, cc.Password)
	}
	if cc.TLSConfig != nil {
		t.Error("sslmode=disable but TLSConfig is set")
	}
	wantParams := map[string]string{"TimeZone": "UTC", "DateStyle": "ISO, MDY", "application_name": "push"}
	if len(cc.RuntimeParams) != len(wantParams) {
		t.Errorf("RuntimeParams = %v, want %v", cc.RuntimeParams, wantParams)
	}
	for k, v := range wantParams {
		if cc.RuntimeParams[k] != v {
			t.Errorf("RuntimeParams[%s] = %q, want %q", k, cc.RuntimeParams[k], v)
		}
	}
	if cc.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want simple protocol", cc.DefaultQueryExecMode)
	}
	if cc.StatementCacheCapacity != 0 || cc.DescriptionCacheCapacity != 0 {
		t.Error("statement/description caches must be disabled")
	}
	if pc.MaxConns != 4 || pc.MinConns != 0 {
		t.Errorf("MaxConns/MinConns = %d/%d, want 4/0", pc.MaxConns, pc.MinConns)
	}
	if pc.MaxConnIdleTime != 5*time.Minute || pc.MaxConnLifetime != 30*time.Minute || pc.HealthCheckPeriod != time.Minute {
		t.Errorf("idle/lifetime/health = %v/%v/%v", pc.MaxConnIdleTime, pc.MaxConnLifetime, pc.HealthCheckPeriod)
	}
	if cc.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout = %v", cc.ConnectTimeout)
	}

	rw, err := cfg.poolConfig("ometria_ecomm_0001", false)
	if err != nil {
		t.Fatal(err)
	}
	if rw.ConnConfig.Host != "127.0.0.1" {
		t.Errorf("read-write host = %s, want 127.0.0.1", rw.ConnConfig.Host)
	}
}

func TestPoolConfigIgnoresPGOptions(t *testing.T) {
	t.Setenv("PGOPTIONS", "-c statement_timeout=5s")
	t.Setenv("PGAPPNAME", "from-env")
	pc, err := testConfig().withDefaults().poolConfig("ometria_ecomm", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pc.ConnConfig.RuntimeParams) != 2 {
		t.Errorf("RuntimeParams = %v, want only TimeZone and DateStyle", pc.ConnConfig.RuntimeParams)
	}
}

func TestPoolCachingConcurrent(t *testing.T) {
	var lookups atomic.Int64
	inner := staticLookup(map[int]string{1: "0001", 2: "0001", 3: "0000", 4: "0002"})
	lookup := ShardLookupFunc(func(ctx context.Context, id int) (Shard, error) {
		lookups.Add(1)
		return inner.LookupShard(ctx, id)
	})
	r, err := NewResolver(testConfig(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	type key struct {
		account  int
		readOnly bool
	}
	const workers = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = map[key]map[*pgxpool.Pool]bool{}
		start   = make(chan struct{})
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			k := key{account: i%4 + 1, readOnly: i%3 == 0}
			p, err := r.Pool(context.Background(), k.account, k.readOnly)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			if results[k] == nil {
				results[k] = map[*pgxpool.Pool]bool{}
			}
			results[k][p] = true
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	for k, pools := range results {
		if len(pools) != 1 {
			t.Errorf("%+v got %d distinct pools, want 1", k, len(pools))
		}
	}
	// Accounts 1 and 2 share shard 0001, so they share pools.
	p1, _ := r.Pool(context.Background(), 1, false)
	p2, _ := r.Pool(context.Background(), 2, false)
	if p1 != p2 {
		t.Error("accounts on the same shard got different pools")
	}
	if ro, _ := r.Pool(context.Background(), 1, true); ro == p1 {
		t.Error("read-only and read-write share a pool")
	}

	var names []string
	for _, s := range r.Stats() {
		names = append(names, s.Database)
		if s.Stat == nil || s.Stat.MaxConns() != DefaultMaxConns {
			t.Errorf("%s: unexpected stat %+v", s.Database, s.Stat)
		}
	}
	want := "ometria_ecomm ometria_ecomm_0001 ometria_ecomm_0001_ro ometria_ecomm_0002 ometria_ecomm_0002_ro ometria_ecomm_ro"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("Stats databases = %q, want %q", got, want)
	}
	if lookups.Load() < workers {
		t.Errorf("lookup called %d times; Resolver should not cache shards itself", lookups.Load())
	}
}

func TestPoolErrors(t *testing.T) {
	r, err := NewResolver(testConfig(), staticLookup(map[int]string{1: ""}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pool(context.Background(), 1, false); !errors.Is(err, ErrNoShard) {
		t.Errorf("empty shard: err = %v, want ErrNoShard", err)
	}
	if _, err := r.Pool(context.Background(), 2, false); err == nil {
		t.Error("lookup failure: want error")
	}
	r.Close()
	r.Close() // idempotent
	if _, err := r.PoolForDatabase("ometria_ecomm"); !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: err = %v, want ErrClosed", err)
	}
	if len(r.Stats()) != 0 {
		t.Error("Stats after Close should be empty")
	}
}
