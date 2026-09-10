package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/iamnikolie/svidoq/internal/datasource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func capture(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	var out, errOut bytes.Buffer

	oldOut, oldErr := stdout, stderr
	stdout, stderr = &out, &errOut

	t.Cleanup(func() {
		stdout, stderr = oldOut, oldErr
		formatFlag, jsonOutput, quiet = "table", false, false
	})

	return &out, &errOut
}

func result() *datasource.Result {
	return &datasource.Result{
		Schema:   "billing",
		Env:      "prod",
		Columns:  []string{"id", "name"},
		Rows:     []map[string]any{{"id": int64(1), "name": "a"}, {"id": int64(2), "name": nil}},
		RowCount: 2,
	}
}

func TestEmitJSONIsCleanOnStdout(t *testing.T) {
	out, errOut := capture(t)
	jsonOutput = true

	require.NoError(t, emit(result()))

	var decoded datasource.Result
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded), "stdout must be parseable JSON and nothing else")
	assert.Equal(t, 2, decoded.RowCount)
	assert.Empty(t, errOut.String(), "no row-count chatter in JSON mode")
}

func TestEmitTableRendersNullDistinctly(t *testing.T) {
	out, _ := capture(t)
	formatFlag = "table"

	require.NoError(t, emit(result()))
	assert.Contains(t, out.String(), "NULL", "a NULL must be distinguishable from an empty string")
}

func TestEmitSingleRowUsesKeyValue(t *testing.T) {
	out, _ := capture(t)
	formatFlag = "table"

	res := result()
	res.Rows = res.Rows[:1]
	res.RowCount = 1

	require.NoError(t, emit(res))
	// Keys are padded to the widest key, so a single row reads as a record.
	assert.Equal(t, "id    1\nname  a\n", out.String())
}

func TestEmitCSV(t *testing.T) {
	out, _ := capture(t)
	formatFlag = "csv"

	require.NoError(t, emit(result()))
	assert.Contains(t, out.String(), "id,name")
}

func TestTruncationIsAnnouncedOnStderr(t *testing.T) {
	out, errOut := capture(t)
	formatFlag = "csv"

	res := result()
	res.Truncated = true

	require.NoError(t, emit(res))
	assert.Contains(t, errOut.String(), "truncated")
	assert.NotContains(t, out.String(), "truncated", "a warning must never land in the data stream")
}

func TestEmitRejectsUnknownFormat(t *testing.T) {
	capture(t)
	formatFlag = "yaml"

	require.Error(t, emit(result()))
}
