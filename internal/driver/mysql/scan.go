package mysql

import "database/sql"

// normalizeCell turns a driver value into a JSON-friendly one: []byte becomes
// a string, everything else (int64, float64, bool, time.Time, nil) passes
// through. Without this every text column renders as a base64 blob in --json.
func normalizeCell(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}

	return v
}

// scanRows reads at most limit rows and reports whether more were available,
// so a truncated result is visibly truncated instead of quietly wrong.
func scanRows(rows *sql.Rows, limit int) (cols []string, out []map[string]any, truncated bool, err error) {
	cols, err = rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}

	for rows.Next() {
		if len(out) == limit {
			truncated = true

			break
		}

		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))

		for i := range raw {
			ptrs[i] = &raw[i]
		}

		if err = rows.Scan(ptrs...); err != nil {
			return nil, nil, false, err
		}

		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeCell(raw[i])
		}

		out = append(out, row)
	}

	if err = rows.Err(); err != nil {
		return nil, nil, false, err
	}

	return cols, out, truncated, nil
}

// quoteLiteral single-quotes a string for an information_schema lookup,
// escaping backslashes and quotes. Only table names and LIKE patterns pass
// through it, and the resulting query still runs inside the read-only
// transaction.
func quoteLiteral(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '\\':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}

	return string(append(out, '\''))
}
