// Package datasource defines the read-only query contract every svidoq driver
// implements. It is a leaf package: drivers import it and the registry imports
// both, so it must not import a concrete driver (that would be an import cycle).
package datasource

import (
	"context"
	"errors"
	"time"
)

// ErrUnknownSchema is returned when a name is not configured in the environment.
var ErrUnknownSchema = errors.New("unknown schema")

// QueryOpts controls a single query execution. Both fields are hard limits:
// the row cap protects the caller's context window and memory, the timeout
// protects the database.
type QueryOpts struct {
	Limit   int
	Timeout time.Duration
}

// Result is a driver-agnostic query result.
type Result struct {
	Schema    string           `json:"schema"`
	Env       string           `json:"env"`
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"row_count"`
	Truncated bool             `json:"truncated"`
	ElapsedMS int64            `json:"elapsed_ms"`
}

// Datasource is a read-only queryable database.
type Datasource interface {
	Name() string
	Driver() string
	Query(ctx context.Context, sql string, opts QueryOpts) (*Result, error)
	Tables(ctx context.Context, like string, opts QueryOpts) (*Result, error)
	Describe(ctx context.Context, table string, opts QueryOpts) (*Result, error)
	Explain(ctx context.Context, sql string, opts QueryOpts) (*Result, error)
	Close() error
}
