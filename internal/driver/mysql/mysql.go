package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/iamnikolie/svidoq/internal/datasource"
)

var (
	// ErrWriteBlocked wraps MySQL error 1792, a write refused by the read-only
	// transaction. Seeing it means layer 1 let something through and layer 2
	// caught it — worth reporting as a bug.
	ErrWriteBlocked = errors.New("write blocked by the read-only transaction")
	// ErrConnectionFailed wraps a failure to acquire a connection.
	ErrConnectionFailed = errors.New("connection failed")
)

// DB is a read-only MySQL/MariaDB datasource.
type DB struct {
	name string
	db   *sql.DB
}

// Open dials a DSN. It does not contact the server; the first query does.
func Open(name, dsn string, connectTimeout time.Duration) (*DB, error) {
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	if connectTimeout > 0 {
		cfg.Timeout = connectTimeout
	}

	connector, err := mysqldriver.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("build connector: %w", err)
	}

	// A read-only CLI never needs concurrency; a small pool keeps the
	// connection count on a shared production server unremarkable.
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	return &DB{name: name, db: db}, nil
}

// Name returns the configured schema name.
func (d *DB) Name() string { return d.name }

// Driver returns the driver identifier.
func (d *DB) Driver() string { return "mysql" }

// Close releases the pool.
func (d *DB) Close() error { return d.db.Close() }

// Query validates that sql is read-only, then runs it.
func (d *DB) Query(ctx context.Context, query string, opts datasource.QueryOpts) (*datasource.Result, error) {
	if err := Validate(query); err != nil {
		return nil, err
	}

	return d.runReadOnly(ctx, query, opts)
}

// Explain runs EXPLAIN over a statement that must itself be read-only, so a
// query plan cannot become a way to smuggle a write past the validator.
func (d *DB) Explain(ctx context.Context, query string, opts datasource.QueryOpts) (*datasource.Result, error) {
	if err := Validate(query); err != nil {
		return nil, err
	}

	return d.runReadOnly(ctx, "EXPLAIN "+query, opts)
}

// Tables lists the tables of the connected database, optionally LIKE-filtered.
func (d *DB) Tables(ctx context.Context, like string, opts datasource.QueryOpts) (*datasource.Result, error) {
	q := "SELECT table_name, table_type, table_rows, engine" +
		" FROM information_schema.tables WHERE table_schema = DATABASE()"

	if like != "" {
		q += " AND table_name LIKE " + quoteLiteral(like)
	}

	return d.runReadOnly(ctx, q+" ORDER BY table_name", opts)
}

// Describe returns column metadata for one table.
func (d *DB) Describe(ctx context.Context, table string, opts datasource.QueryOpts) (*datasource.Result, error) {
	q := "SELECT column_name, column_type, is_nullable, column_key, column_default, extra" +
		" FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = " +
		quoteLiteral(table) + " ORDER BY ordinal_position"

	return d.runReadOnly(ctx, q, opts)
}

// runReadOnly is the only path to the server. Every caller goes through it, so
// the read-only transaction and the timeouts cannot be forgotten at a call site.
func (d *DB) runReadOnly(ctx context.Context, query string, opts datasource.QueryOpts) (*datasource.Result, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()

	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnectionFailed, err)
	}
	defer conn.Close()

	if opts.Timeout > 0 {
		// Server-side deadline as well as the client one: it also neutralizes
		// SLEEP() and a plan that goes quadratic after the rows start flowing.
		// Best effort — MariaDB spells this differently and simply ignores it.
		_, _ = conn.ExecContext(ctx,
			fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d", opts.Timeout.Milliseconds()))
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, mapExecError(err)
	}
	defer rows.Close()

	cols, data, truncated, err := scanRows(rows, opts.Limit)
	if err != nil {
		return nil, mapExecError(err)
	}

	return &datasource.Result{
		Schema:    d.name,
		Columns:   cols,
		Rows:      data,
		RowCount:  len(data),
		Truncated: truncated,
		ElapsedMS: time.Since(start).Milliseconds(),
	}, nil
}

// mapExecError translates MySQL 1792 into ErrWriteBlocked so the CLI can say
// which layer refused the statement.
func mapExecError(err error) error {
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) && me.Number == 1792 {
		return fmt.Errorf("%w: %v", ErrWriteBlocked, err)
	}

	return err
}
