package ecomm

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The production ecomm replicas' values, which PHP's connections inherit
// because ometria.common sets neither parameter.
const (
	DefaultTimeZone  = "UTC"
	DefaultDateStyle = "ISO, MDY"
)

// Pool defaults applied when the corresponding Config field is zero.
const (
	DefaultMaxConns          = 4
	DefaultMaxConnIdleTime   = 5 * time.Minute
	DefaultMaxConnLifetime   = 30 * time.Minute
	DefaultHealthCheckPeriod = time.Minute
	DefaultConnectTimeout    = 5 * time.Second
)

// Config describes how to reach the ecomm shards and how to size each
// shard's pool. There are no globals: build a Resolver per Config.
type Config struct {
	// Host and Port of PgBouncer. Required.
	Host string
	Port uint16
	// ReadOnlyHost and ReadOnlyPort, when set, route "_ro" databases to a
	// separate PgBouncer (e.g. pgbouncer-readonly). They default to Host and
	// Port, which also serves the "_ro" databases, as PHP's config does.
	ReadOnlyHost string
	ReadOnlyPort uint16

	User     string // Required.
	Password string
	// SSLMode is a libpq sslmode: disable, allow, prefer, require,
	// verify-ca or verify-full. Required, so TLS is a deliberate choice.
	SSLMode string

	// TimeZone and DateStyle are sent as startup parameters on every
	// connection. Required; use DefaultTimeZone and DefaultDateStyle to
	// match PHP.
	TimeZone  string
	DateStyle string
	// ApplicationName is sent as application_name when set.
	ApplicationName string

	// MaxConns per database (default 4) and MinConns (default 0, so pools
	// for quiet shards drain to nothing).
	MaxConns int32
	MinConns int32
	// MaxConnIdleTime defaults to 5m, MaxConnLifetime to 30m (with up to
	// 10% jitter), HealthCheckPeriod to 1m and ConnectTimeout to 5s.
	MaxConnIdleTime   time.Duration
	MaxConnLifetime   time.Duration
	HealthCheckPeriod time.Duration
	ConnectTimeout    time.Duration
}

var validSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

func (c Config) validate() error {
	var errs []error
	if c.Host == "" {
		errs = append(errs, errors.New("Host is required"))
	}
	if c.Port == 0 {
		errs = append(errs, errors.New("Port is required"))
	}
	if c.User == "" {
		errs = append(errs, errors.New("User is required"))
	}
	if !validSSLModes[c.SSLMode] {
		errs = append(errs, fmt.Errorf("SSLMode %q is not a libpq sslmode", c.SSLMode))
	}
	if c.TimeZone == "" {
		errs = append(errs, errors.New("TimeZone is required"))
	}
	if c.DateStyle == "" {
		errs = append(errs, errors.New("DateStyle is required"))
	}
	if c.MaxConns < 0 || c.MinConns < 0 {
		errs = append(errs, errors.New("MaxConns and MinConns must not be negative"))
	}
	if c.MaxConns > 0 && c.MinConns > c.MaxConns {
		errs = append(errs, errors.New("MinConns must not exceed MaxConns"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("ecomm: invalid config: %w", errors.Join(errs...))
	}
	return nil
}

func (c Config) withDefaults() Config {
	if c.ReadOnlyHost == "" {
		c.ReadOnlyHost = c.Host
	}
	if c.ReadOnlyPort == 0 {
		c.ReadOnlyPort = c.Port
	}
	if c.MaxConns == 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.MaxConnIdleTime == 0 {
		c.MaxConnIdleTime = DefaultMaxConnIdleTime
	}
	if c.MaxConnLifetime == 0 {
		c.MaxConnLifetime = DefaultMaxConnLifetime
	}
	if c.HealthCheckPeriod == 0 {
		c.HealthCheckPeriod = DefaultHealthCheckPeriod
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	return c
}

// poolConfig builds the pgxpool config for one database. c must already
// have defaults applied.
func (c Config) poolConfig(dbName string, readOnly bool) (*pgxpool.Config, error) {
	host, port := c.Host, c.Port
	if readOnly {
		host, port = c.ReadOnlyHost, c.ReadOnlyPort
	}
	// The keyword/value form lets pgconn build the TLS config for sslmode.
	// Explicit keys win over PG* environment variables.
	dsn := strings.Join([]string{
		kv("host", host),
		kv("port", fmt.Sprint(port)),
		kv("dbname", dbName),
		kv("user", c.User),
		kv("password", c.Password),
		kv("sslmode", c.SSLMode),
	}, " ")
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("ecomm: pool config for %s: %w", dbName, err)
	}

	cc := pc.ConnConfig
	cc.ConnectTimeout = c.ConnectTimeout
	// Replace, not merge: anything else (PGOPTIONS, PGAPPNAME, ...) could
	// be a startup parameter PgBouncer rejects.
	cc.RuntimeParams = map[string]string{
		"TimeZone":  c.TimeZone,
		"DateStyle": c.DateStyle,
	}
	if c.ApplicationName != "" {
		cc.RuntimeParams["application_name"] = c.ApplicationName
	}
	// Transaction pooling: no named or cached server-side statements.
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cc.StatementCacheCapacity = 0
	cc.DescriptionCacheCapacity = 0

	pc.MaxConns = c.MaxConns
	pc.MinConns = c.MinConns
	pc.MaxConnIdleTime = c.MaxConnIdleTime
	pc.MaxConnLifetime = c.MaxConnLifetime
	pc.MaxConnLifetimeJitter = c.MaxConnLifetime / 10
	pc.HealthCheckPeriod = c.HealthCheckPeriod
	return pc, nil
}

// kv quotes a libpq keyword/value pair.
func kv(k, v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return k + "='" + v + "'"
}
