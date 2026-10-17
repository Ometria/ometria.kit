// Package ecomm is kit v2's tenant-database layer: it routes an account to
// its ecomm shard database and hands out a pgx connection pool for it, the
// way PHP's database('ecomm', $account, $read_only) does in ometria.common.
//
// It is a separate Go module so services that never touch Postgres do not
// pull pgx into their build.
//
//	resolver, err := ecomm.NewResolver(ecomm.Config{
//		Host:      "pgbouncer2",
//		Port:      5432,
//		User:      "ometria",
//		Password:  password,
//		SSLMode:   "disable",
//		TimeZone:  ecomm.DefaultTimeZone,
//		DateStyle: ecomm.DefaultDateStyle,
//	}, lookup)
//	defer resolver.Close()
//
//	pool, err := resolver.Pool(ctx, accountID, true) // ometria_ecomm_NNNN_ro
//
// # Database names
//
// The name is derived from the account's db_shard, as PHP does; db_name from
// api-accounts is not used for routing (api-accounts derives it the same
// way). Shard "0001" maps to ometria_ecomm_0001, and the default shard
// "0000" maps to ometria_ecomm. Read-only appends "_ro":
// ometria_ecomm_0001_ro and ometria_ecomm_ro.
//
// # PgBouncer
//
// The databases sit behind PgBouncer in transaction-pool mode, and every
// session parameter is sent as a startup parameter. PgBouncer tracks
// TimeZone and DateStyle itself and re-applies the client's values on
// whichever server connection it hands out, which is what makes pinning them
// safe in transaction mode. Any other startup parameter, such as
// statement_timeout, is rejected by PgBouncer unless it is listed in
// track_extra_parameters or ignore_startup_parameters; use context deadlines
// for query timeouts instead. Callers must set one: the production ometria
// role has statement_timeout=6h, so the server will not stop a slow query.
//
// # Simple protocol
//
// Pools use pgx's simple query protocol, with parameters interpolated on the
// client and no statement or description caches. That is not forced by
// PgBouncer, which has tracked prepared statements in transaction mode since
// 1.21 (max_prepared_statements); it is chosen to match PHP, whose PDO
// connections emulate prepares and read every value back as text, so a Go
// port sends the same SQL and sees the same values. It costs:
//
//   - Results arrive in text format and pgx parses them on the client, which
//     is slower than binary for wide or numeric-heavy rows.
//   - Timestamps are parsed from DateStyle output, and pgx only understands
//     the ISO style, so Config requires a DateStyle that starts with ISO.
//   - SendBatch sends the whole batch as one multi-statement simple query,
//     which Postgres runs as a single implicit transaction: a failing
//     statement rolls back the earlier ones unless the batch has its own
//     BEGIN/COMMIT.
//
// # Timestamps
//
// TimeZone and DateStyle are required. PHP's PDO reads timestamps as Postgres
// text in the server's defaults, so a Go port that formats ::text or relies
// on to_char output must pin the same values to produce identical
// responses. DefaultTimeZone ("UTC") and DefaultDateStyle ("ISO, MDY") are
// what production ecomm replicas use (TimeZone from the server configuration
// file, DateStyle the built-in default, with no role or database override),
// which PHP inherits because it sets neither.
package ecomm
