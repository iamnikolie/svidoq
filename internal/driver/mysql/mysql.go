package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
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
	name      string
	db        *sql.DB
	multiStmt bool

	// capWarned makes the "no server-side cap" warning appear once per
	// process, not once per query.
	capWarned sync.Once
	// noServerCap skips the server-side cap, so an integration test can prove
	// the KILL QUERY path on its own.
	noServerCap bool
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

	// Refuse multi-statement mode whatever the DSN asked for. The validator
	// already rejects two statements in one call, but a driver that cannot
	// send them is a second wall that no future code path can walk around —
	// and a read-only tool has no use for the feature.
	cfg.MultiStatements = false

	// Interpolation would build the final SQL client-side, out of reach of the
	// server's own parsing. Keep statements going over the wire as written.
	cfg.InterpolateParams = false

	connector, err := mysqldriver.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("build connector: %w", err)
	}

	// A read-only CLI never needs concurrency; a small pool keeps the
	// connection count on a shared production server unremarkable.
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	return &DB{name: name, db: db, multiStmt: cfg.MultiStatements}, nil
}

// multiStatements reports the setting the pool was actually opened with, so a
// test can assert the DSN could not re-enable it.
func (d *DB) multiStatements() bool { return d.multiStmt }

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

	var connID uint64

	if opts.Timeout > 0 {
		if connID, err = d.capStatementTime(ctx, conn, opts.Timeout); err != nil {
			return nil, err
		}
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, d.queryError(ctx, err, connID, opts.Timeout)
	}
	defer rows.Close()

	cols, data, truncated, err := scanRows(rows, opts.Limit)
	if err != nil {
		return nil, d.queryError(ctx, err, connID, opts.Timeout)
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

// capStatementTime sets the server-side statement cap for this connection and
// returns the connection id, which KILL QUERY needs if the client deadline
// fires first. It runs before BeginTx: SET is issued by svidoq itself, never
// by the user, so the validator's SET ban does not apply.
func (d *DB) capStatementTime(ctx context.Context, conn *sql.Conn, timeout time.Duration) (uint64, error) {
	var (
		id      uint64
		version string
	)

	probeStart := time.Now()

	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID(), VERSION()").Scan(&id, &version); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrConnectionFailed, err)
	}

	// The server starts counting when the statement starts, the client when
	// the connection was requested. Over a slow link the connect and the two
	// round trips still to come (SET, BEGIN) eat a real share of the budget.
	left := timeout
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline) - 2*time.Since(probeStart)
	}

	if d.noServerCap || left <= 0 {
		return id, nil
	}

	stmt := timeoutStatement(version, serverCap(left))

	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		if !isUnknownVariable(err) {
			return 0, fmt.Errorf("set statement timeout: %w", err)
		}

		// An old server without either variable. The query still runs, and
		// KILL QUERY after the client deadline stops it — but say so.
		d.capWarned.Do(func() {
			warnf("server %s has no statement timeout (%v); --timeout is enforced by KILL QUERY only", version, err)
		})
	}

	return id, nil
}

// queryError maps a failed query. When the client deadline (or a cancel) got
// there first, the server may still be executing: stop it with KILL QUERY.
func (d *DB) queryError(ctx context.Context, err error, connID uint64, timeout time.Duration) error {
	if isServerTimeout(err) {
		return timeoutError(timeout, "stopped by the server")
	}

	if ctx.Err() == nil || connID == 0 {
		return mapExecError(err)
	}

	cause := "KILL QUERY sent"
	if kerr := d.killQuery(connID); kerr != nil {
		cause = fmt.Sprintf("KILL QUERY %d failed (%v); check PROCESSLIST", connID, kerr)
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeoutError(timeout, "client deadline, "+cause)
	}

	return fmt.Errorf("%w (%s)", ctx.Err(), cause)
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
