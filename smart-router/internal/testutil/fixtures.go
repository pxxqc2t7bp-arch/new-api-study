package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// OwnedDir returns a private canonical directory with testing-managed cleanup.
func OwnedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0700))
	return dir
}

// Python resolves the selected test interpreter without weakening runtime pins.
func Python(t *testing.T) string {
	t.Helper()
	path := os.Getenv("MODELTRACE_TEST_PYTHON")
	if path == "" {
		var err error
		path, err = exec.LookPath("python3")
		require.NoError(t, err, "install Python 3 or set MODELTRACE_TEST_PYTHON")
	}
	path, err := filepath.Abs(path)
	require.NoError(t, err)
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	require.NoError(t, err, "test interpreter directory must exist")
	path = filepath.Join(dir, filepath.Base(path))
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err, "test interpreter must exist")
	if resolved != path {
		// Invocation location selects pyvenv.cfg; resolving the executable would lose it.
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "pyvenv.cfg")); !os.IsNotExist(err) {
			require.NoError(t, err, "test virtualenv configuration must be readable")
			t.Fatal("symlink-based virtualenv interpreters are unsupported; recreate with python3 -m venv --copies and set MODELTRACE_TEST_PYTHON")
		}
	}
	path = resolved
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "test interpreter must be a regular file")
	return path
}
