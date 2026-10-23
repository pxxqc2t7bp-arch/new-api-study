package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPythonSelection(t *testing.T) {
	if os.Getenv("MODELTRACE_FIXTURE_CHILD") == "1" {
		selected := Python(t)
		if os.Getenv("MODELTRACE_FIXTURE_PREFIX") == "" {
			return
		}
		output, err := exec.Command(selected, "-c", "import sys; print(sys.prefix)").CombinedOutput()
		require.NoError(t, err, string(output))
		require.Equal(t, os.Getenv("MODELTRACE_FIXTURE_PREFIX"), strings.TrimSpace(string(output)))
		return
	}

	python := Python(t)
	root := OwnedDir(t)
	for _, mode := range []string{"copies", "symlinks"} {
		output, err := exec.Command(python, "-m", "venv", "--without-pip", "--"+mode,
			filepath.Join(root, mode)).CombinedOutput()
		require.NoError(t, err, string(output))
	}
	baseOutput, err := exec.Command(python, "-c", "import sys; print(sys._base_executable); print(sys.base_prefix)").CombinedOutput()
	require.NoError(t, err, string(baseOutput))
	base := strings.Split(strings.TrimSpace(string(baseOutput)), "\n")
	require.Len(t, base, 2)
	aliasDir := filepath.Join(root, "base-alias")
	require.NoError(t, os.Mkdir(aliasDir, 0700))
	require.NoError(t, os.Symlink(base[0], filepath.Join(aliasDir, "python3")))
	testBinary, err := os.Executable()
	require.NoError(t, err)

	for _, test := range []struct {
		name     string
		override string
		path     string
		prefix   string
		rejected bool
	}{
		{"copies-override", filepath.Join(root, "copies/bin/python3"), "", filepath.Join(root, "copies"), false},
		{"empty-override-path", "", filepath.Join(root, "copies/bin"), filepath.Join(root, "copies"), false},
		{"symlink-override", filepath.Join(root, "symlinks/bin/python3"), "", "", true},
		{"symlink-path", "", filepath.Join(root, "symlinks/bin"), "", true},
		{"base-install-alias", filepath.Join(aliasDir, "python3"), "", base[1], false},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := exec.Command(testBinary, "-test.run=^TestPythonSelection$", "-test.count=1")
			child.Env = []string{
				"PATH=" + test.path,
				"MODELTRACE_TEST_PYTHON=" + test.override,
				"MODELTRACE_FIXTURE_CHILD=1",
				"MODELTRACE_FIXTURE_PREFIX=" + test.prefix,
				"PYTHONDONTWRITEBYTECODE=1",
			}
			output, err := child.CombinedOutput()
			if test.rejected {
				require.Error(t, err, string(output))
				assert.Contains(t, string(output), "symlink-based virtualenv")
				assert.Contains(t, string(output), "--copies")
			} else {
				require.NoError(t, err, string(output))
			}
		})
	}
}
