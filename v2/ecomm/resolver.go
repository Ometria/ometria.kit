package ecomm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrClosed is returned by Pool after Close.
var ErrClosed = errors.New("ecomm: resolver closed")

// Resolver hands out one lazily created pgx pool per shard database. It is
// safe for concurrent use.
type Resolver struct {
	cfg    Config
	lookup ShardLookup

	mu     sync.Mutex
	pools  map[string]*pgxpool.Pool
	closed bool
}

// NewResolver validates cfg and returns a Resolver. It opens no connections.
func NewResolver(cfg Config, lookup ShardLookup) (*Resolver, error) {
	if lookup == nil {
		return nil, errors.New("ecomm: ShardLookup is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	// Surface environment problems (PGSERVICE) now rather than on first use.
	if _, err := cfg.poolConfig("ometria_ecomm", false); err != nil {
		return nil, err
	}
	return &Resolver{
		cfg:    cfg,
		lookup: lookup,
		pools:  make(map[string]*pgxpool.Pool),
	}, nil
}

// Pool returns the pool for the account's ecomm shard, or its read replica
// when readOnly. The pool is shared: callers must not close it.
func (r *Resolver) Pool(ctx context.Context, accountID int, readOnly bool) (*pgxpool.Pool, error) {
	shard, err := r.lookup.LookupShard(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("ecomm: look up shard for account %d: %w", accountID, err)
	}
	name, err := DatabaseName(shard.DbShard, readOnly)
	if err != nil {
		return nil, fmt.Errorf("account %d: %w", accountID, err)
	}
	return r.PoolForDatabase(name)
}

// PoolForDatabase returns the pool for a database name as produced by
// DatabaseName, creating it on first use. Names ending in "_ro" use the
// read-only host.
func (r *Resolver) PoolForDatabase(name string) (*pgxpool.Pool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if p, ok := r.pools[name]; ok {
		return p, nil
	}
	pc, err := r.cfg.poolConfig(name, strings.HasSuffix(name, "_ro"))
	if err != nil {
		return nil, err
	}
	// NewWithConfig does not dial (MinConns are filled in the background),
	// so creating under the lock is cheap and guarantees one pool per name.
	p, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, fmt.Errorf("ecomm: create pool for %s: %w", name, err)
	}
	r.pools[name] = p
	return p, nil
}

// PoolStats is a snapshot of one database's pool, for metrics.
type PoolStats struct {
	Database string
	Stat     *pgxpool.Stat
}

// Stats returns a snapshot of every pool created so far, sorted by database
// name. Export it with whatever metrics library the service uses.
func (r *Resolver) Stats() []PoolStats {
	r.mu.Lock()
	out := make([]PoolStats, 0, len(r.pools))
	for name, p := range r.pools {
		out = append(out, PoolStats{Database: name, Stat: p.Stat()})
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b PoolStats) int { return strings.Compare(a.Database, b.Database) })
	return out
}

// Close closes every pool, waiting for acquired connections to be released.
// Pool returns ErrClosed afterwards. Close is idempotent.
func (r *Resolver) Close() {
	r.mu.Lock()
	pools := r.pools
	r.pools = map[string]*pgxpool.Pool{}
	r.closed = true
	r.mu.Unlock()
	for _, p := range pools {
		p.Close()
	}
}
