package ecomm

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DefaultShard is the shard whose database has no numeric suffix.
const DefaultShard = "0000"

// Shard is where an account's ecomm data lives, as api-accounts reports it.
type Shard struct {
	// DbShard is the shard id, e.g. "0001". It decides the database name.
	DbShard string
	// DbName is api-accounts' db_name, e.g. "ometria_ecomm_0001". It is
	// informational only: like PHP, routing derives the name from DbShard.
	DbName string
}

// ShardLookup maps an account to its shard. The accounts client in a
// service, which already caches api-accounts metadata, usually implements it.
type ShardLookup interface {
	LookupShard(ctx context.Context, accountID int) (Shard, error)
}

// ShardLookupFunc adapts a function to ShardLookup.
type ShardLookupFunc func(ctx context.Context, accountID int) (Shard, error)

// LookupShard calls f.
func (f ShardLookupFunc) LookupShard(ctx context.Context, accountID int) (Shard, error) {
	return f(ctx, accountID)
}

// ErrNoShard is returned when an account has no db_shard, matching PHP's
// "Cannot work out shard for account" error.
var ErrNoShard = errors.New("ecomm: cannot work out shard for account")

// DatabaseName returns the PgBouncer database name for a shard, following
// ometria.common's _do_database_get_adaptor: "ometria_ecomm_" + shard, with
// shard "0000" mapping to "ometria_ecomm", plus "_ro" when readOnly.
//
// Shard ids must be non-empty and contain only ASCII letters, digits and
// underscores, so a bad api-accounts value cannot produce a surprising name.
func DatabaseName(shard string, readOnly bool) (string, error) {
	if shard == "" {
		return "", ErrNoShard
	}
	for _, r := range shard {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_') {
			return "", fmt.Errorf("ecomm: invalid shard id %q", shard)
		}
	}
	name := "ometria_ecomm_" + shard
	if shard == DefaultShard {
		name = "ometria_ecomm"
	}
	if readOnly {
		name += "_ro"
	}
	return name, nil
}

// parseDatabaseName reports whether name is one DatabaseName can produce,
// and whether it is a read-only name.
func parseDatabaseName(name string) (readOnly bool, ok bool) {
	base, readOnly := strings.CutSuffix(name, "_ro")
	if base == "ometria_ecomm" {
		return readOnly, true
	}
	shard, found := strings.CutPrefix(base, "ometria_ecomm_")
	if !found {
		return false, false
	}
	got, err := DatabaseName(shard, readOnly)
	return readOnly, err == nil && got == name
}
