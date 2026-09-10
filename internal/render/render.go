// Package render writes a result set as an aligned table, CSV or TSV.
package render

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Cell converts a scanned value into its display form. NULL is rendered as an
// unquoted NULL so it is distinguishable from the empty string, which is the
// distinction that matters most when reading a table by eye.
func Cell(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case time.Time:
		return t.Format(time.RFC3339)
	case []byte:
		return string(t)
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// Table writes an aligned text table. Newlines and tabs inside a value are
// escaped so one wrapped cell cannot destroy the alignment of the whole table.
func Table(w io.Writer, cols []string, rows []map[string]any) error {
	if len(cols) == 0 {
		return nil
	}

	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c)
	}

	cells := make([][]string, 0, len(rows))

	for _, row := range rows {
		line := make([]string, len(cols))

		for i, c := range cols {
			s := escape(Cell(row[c]))
			line[i] = s

			if n := utf8.RuneCountInString(s); n > widths[i] {
				widths[i] = n
			}
		}

		cells = append(cells, line)
	}

	if err := writeLine(w, cols, widths); err != nil {
		return err
	}

	sep := make([]string, len(cols))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}

	if err := writeLine(w, sep, widths); err != nil {
		return err
	}

	for _, line := range cells {
		if err := writeLine(w, line, widths); err != nil {
			return err
		}
	}

	return nil
}

func writeLine(w io.Writer, values []string, widths []int) error {
	parts := make([]string, len(values))

	for i, v := range values {
		pad := widths[i] - utf8.RuneCountInString(v)
		if pad < 0 {
			pad = 0
		}

		parts[i] = v + strings.Repeat(" ", pad)
	}

	_, err := fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))

	return err
}

func escape(s string) string {
	return strings.NewReplacer("\n", "\\n", "\r", "\\r", "\t", "\\t").Replace(s)
}

// KV writes one record as aligned key/value lines — the readable shape for a
// single row, where a table would be one very wide line.
func KV(w io.Writer, cols []string, row map[string]any) error {
	width := 0

	for _, c := range cols {
		if n := utf8.RuneCountInString(c); n > width {
			width = n
		}
	}

	for _, c := range cols {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(c))
		if _, err := fmt.Fprintf(w, "%s%s  %s\n", c, pad, escape(Cell(row[c]))); err != nil {
			return err
		}
	}

	return nil
}

// Delimited writes CSV (comma) or TSV (tab). Values keep their real newlines
// here: both formats quote them, and the consumer is a parser, not an eye.
func Delimited(w io.Writer, cols []string, rows []map[string]any, comma rune) error {
	cw := csv.NewWriter(w)
	cw.Comma = comma

	if err := cw.Write(cols); err != nil {
		return err
	}

	for _, row := range rows {
		line := make([]string, len(cols))
		for i, c := range cols {
			line[i] = Cell(row[c])
		}

		if err := cw.Write(line); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}
