package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Version must land on stdout: `v=$(svidoq version)` has to work.
func TestVersionGoesToStdout(t *testing.T) {
	out, errOut := capture(t)

	require.NoError(t, versionCmd.RunE(versionCmd, nil))
	assert.Contains(t, out.String(), "svidoq ")
	assert.Empty(t, errOut.String())
}
