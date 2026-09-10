// Package mysql implements the read-only MySQL/MariaDB driver.
//
// Read-only is guaranteed twice over, by two mechanisms that share no code:
//
//  1. Validate parses the statement with the TiDB MySQL parser and accepts only
//     read node types. A parser is used rather than a prefix or regexp check
//     because "SELECT" is not the same thing as "read": SELECT ... INTO OUTFILE
//     writes to the filesystem, and a semicolon turns one statement into two.
//  2. Every statement runs inside START TRANSACTION READ ONLY, so the server
//     rejects a write (error 1792) even if layer 1 were ever bypassed.
//
// Neither layer is a substitute for a SELECT-only database grant where you can
// get one. They exist for the common case where you cannot: a shared
// application credential you do not own.
package mysql

import (
	"errors"
	"fmt"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver" // registers the value-expr driver for standalone parsing
)

// ErrNotReadOnly marks a statement rejected by the read-only validator.
var ErrNotReadOnly = errors.New("statement is not read-only")

// Validate returns nil only when sql is a single read-only statement.
func Validate(sql string) error {
	stmts, _, err := parser.New().Parse(sql, "", "")
	if err != nil {
		return fmt.Errorf("%w: cannot parse SQL: %v", ErrNotReadOnly, err)
	}

	switch len(stmts) {
	case 0:
		return fmt.Errorf("%w: no statement found", ErrNotReadOnly)
	case 1:
	default:
		return fmt.Errorf("%w: multiple statements are not allowed (found %d)", ErrNotReadOnly, len(stmts))
	}

	switch s := stmts[0].(type) {
	case *ast.SelectStmt:
		// SELECT ... INTO OUTFILE / DUMPFILE writes to the server's filesystem.
		if s.SelectIntoOpt != nil {
			return fmt.Errorf("%w: SELECT ... INTO writes to the filesystem", ErrNotReadOnly)
		}

		// A locking read changes no rows but takes locks, which on a busy
		// server stalls the writers. "Reads nothing but blocks production" is
		// not the promise this tool makes.
		if s.LockInfo != nil && s.LockInfo.LockType != ast.SelectLockNone {
			return fmt.Errorf("%w: a locking read (FOR UPDATE / FOR SHARE / LOCK IN SHARE MODE) blocks writers", ErrNotReadOnly)
		}

		return nil
	case *ast.SetOprStmt: // UNION / INTERSECT / EXCEPT over selects
		return nil
	case *ast.ShowStmt:
		return nil
	case *ast.ExplainStmt: // covers EXPLAIN and DESCRIBE
		return nil
	default:
		return fmt.Errorf("%w: %s is a write or a session change", ErrNotReadOnly, stmtKind(stmts[0]))
	}
}

// stmtKind names the rejected statement in the error, because "not read-only"
// alone leaves the caller guessing which part of a long query was the problem.
func stmtKind(n ast.StmtNode) string {
	switch n.(type) {
	case *ast.InsertStmt:
		return "INSERT/REPLACE"
	case *ast.UpdateStmt:
		return "UPDATE"
	case *ast.DeleteStmt:
		return "DELETE"
	case *ast.CreateTableStmt, *ast.CreateDatabaseStmt, *ast.CreateIndexStmt, *ast.CreateViewStmt:
		return "CREATE"
	case *ast.DropTableStmt, *ast.DropDatabaseStmt, *ast.DropIndexStmt:
		return "DROP"
	case *ast.AlterTableStmt, *ast.AlterDatabaseStmt:
		return "ALTER"
	case *ast.TruncateTableStmt:
		return "TRUNCATE"
	case *ast.RenameTableStmt:
		return "RENAME"
	case *ast.GrantStmt, *ast.RevokeStmt, *ast.CreateUserStmt, *ast.DropUserStmt, *ast.SetPwdStmt:
		return "a privilege statement"
	case *ast.SetStmt, *ast.SetSessionStatesStmt:
		return "SET"
	case *ast.UseStmt:
		return "USE"
	case *ast.LoadDataStmt:
		return "LOAD DATA"
	case *ast.CallStmt:
		return "CALL"
	case *ast.BeginStmt, *ast.CommitStmt, *ast.RollbackStmt:
		return "a transaction statement"
	case *ast.LockTablesStmt, *ast.UnlockTablesStmt:
		return "LOCK/UNLOCK TABLES"
	case *ast.FlushStmt:
		return "FLUSH"
	case *ast.KillStmt:
		return "KILL"
	default:
		return fmt.Sprintf("%T", n)
	}
}
