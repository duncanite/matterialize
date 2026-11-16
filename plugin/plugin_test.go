package plugin_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/duncanite/matterialize/plugin"
)

// A secret in a file is found by its name in the directory of secrets,
// and by nothing that leads out of it.
func TestWhereASecretIs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	secrets := plugin.Secrets{Dir: dir}

	if got, err := secrets.Path("lights"); err != nil || got != filepath.Join(dir, "lights") {
		t.Errorf("the secret lights is at %q, %v", got, err)
	}

	for _, name := range []string{"", "..", "../lights", "a/b", "."} {
		if got, err := secrets.Path(name); !errors.Is(err, plugin.ErrSettings) {
			t.Errorf("the secret %q is at %q, %v", name, got, err)
		}
	}

	if got, err := (plugin.Secrets{}).Path("lights"); !errors.Is(err, plugin.ErrSettings) {
		t.Errorf("with no directory of secrets, lights is at %q, %v", got, err)
	}
}

// A secret of the environment is the variable's value, and its absence
// is an error.
func TestASecretOfTheEnvironment(t *testing.T) {
	t.Setenv("MATTERIALIZE_TEST_SECRET", "hunter2")
	t.Setenv("MATTERIALIZE_TEST_EMPTY", "")

	var secrets plugin.Secrets

	if got, err := secrets.Env("MATTERIALIZE_TEST_SECRET"); err != nil || got != "hunter2" {
		t.Errorf("read %q, %v", got, err)
	}

	for _, name := range []string{"MATTERIALIZE_TEST_EMPTY", "MATTERIALIZE_TEST_UNSET"} {
		if got, err := secrets.Env(name); !errors.Is(err, plugin.ErrSettings) || got != "" {
			t.Errorf("%s read as %q, %v", name, got, err)
		}
	}
}
