package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/iamnikolie/svidoq/internal/datasource"
	"github.com/iamnikolie/svidoq/internal/render"
)

// emit writes a result in the requested format and reports truncation.
//
// The truncation notice goes to stderr, not stdout: a caller piping into jq
// must not have a warning land in the middle of its JSON, but a caller reading
// the output must not silently believe it saw every row.
func emit(res *datasource.Result) error {
	format := formatFlag
	if jsonOutput {
		format = "json"
	}

	switch format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(res); err != nil {
			return err
		}
	case "csv":
		if err := render.Delimited(stdout, res.Columns, res.Rows, ','); err != nil {
			return err
		}
	case "tsv":
		if err := render.Delimited(stdout, res.Columns, res.Rows, '\t'); err != nil {
			return err
		}
	case "table", "":
		if len(res.Rows) == 1 {
			if err := render.KV(stdout, res.Columns, res.Rows[0]); err != nil {
				return err
			}

			break
		}

		if err := render.Table(stdout, res.Columns, res.Rows); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown --format %q (table|json|csv|tsv)", format)
	}

	if !quiet && format != "json" {
		fmt.Fprintf(stderr, "%d row(s) in %dms\n", res.RowCount, res.ElapsedMS)
	}

	if res.Truncated {
		fmt.Fprintf(stderr,
			"truncated at %d rows — narrow the query, or raise --limit up to the environment's max_limit\n",
			res.RowCount)
	}

	return nil
}
