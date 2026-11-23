package command_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duncanite/matterialize/internal/command"
	"github.com/duncanite/matterialize/internal/daemon"
	"github.com/duncanite/matterialize/internal/store"
	"github.com/duncanite/matterialize/plugin"
)

// built is the plugins of a test's build: one, which is never made.
func built() map[string]plugin.Maker {
	return map[string]plugin.Maker{"lights": nil}
}

// configured writes a configuration whose store is a directory of the
// test's, and gives both.
func configured(t *testing.T, kept string) (path, dir string) {
	t.Helper()

	dir = filepath.Join(t.TempDir(), "store")
	path = filepath.Join(t.TempDir(), "config.yaml")

	written := "bridge: {name: Test, state: " + filepath.Join(t.TempDir(), "state") + "}\nplugins: {lights: {}}\n"
	if kept != "" {
		written += "store: " + dir + "\n"
	}

	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}

	return path, dir
}

// A secret given on standard input is kept for a plugin under a name,
// whole and as it is, where that plugin's store reads it, in place of
// what was there, and is not said back.
func TestASecretIsKept(t *testing.T) {
	t.Parallel()

	path, dir := configured(t, "kept")

	for _, secret := range []string{"first\n", "a key\nand what goes with it\n"} {
		var out bytes.Buffer

		err := command.Run(
			t.Context(),
			[]string{"keep", path, "lights", "token"},
			built(),
			strings.NewReader(secret),
			&out,
		)
		if err != nil {
			t.Fatal(err)
		}

		if said := out.String(); said != "kept token for the plugin lights\n" || strings.Contains(said, "key") {
			t.Errorf("it said %q", said)
		}

		held, err := store.Dir(dir).For("lights").Get(t.Context(), "token")
		if err != nil || string(held) != secret {
			t.Errorf("the plugin's store holds %q, %v", held, err)
		}
	}

	// It is the plugin's alone to read, as everything the store keeps.
	if runtimeHasModes() {
		info, err := os.Stat(filepath.Join(dir, "lights", "token"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the secret is kept as %v, %v", info, err)
		}
	}
}

// What cannot be kept is refused, and nothing is written: no secret,
// one that is more than a secret is, a plugin this build does not have,
// a name that is not one, a configuration with no store, and a command
// asked wrongly.
func TestWhatIsNotKept(t *testing.T) {
	t.Parallel()

	path, dir := configured(t, "kept")
	bare, _ := configured(t, "")

	for what, row := range map[string]struct {
		args   []string
		secret string
		want   error
	}{
		"no secret":           {[]string{"keep", path, "lights", "token"}, "", command.ErrSecret},
		"more than a secret":  {[]string{"keep", path, "lights", "token"}, strings.Repeat("x", 64<<10+1), command.ErrSecret},
		"no such plugin":      {[]string{"keep", path, "plugs", "token"}, "x", daemon.ErrNoPlugin},
		"a name that is none": {[]string{"keep", path, "lights", "../token"}, "x", store.ErrName},
		"no store":            {[]string{"keep", bare, "lights", "token"}, "x", store.ErrNone},
		"no configuration":    {[]string{"keep", filepath.Join(dir, "none.yaml"), "lights", "token"}, "x", os.ErrNotExist},
		"a word too few":      {[]string{"keep", path, "lights"}, "x", command.ErrUsage},
		"a word too many":     {[]string{"keep", path, "lights", "token", "more"}, "x", command.ErrUsage},
		"nothing asked":       {nil, "x", command.ErrUsage},
		"keep with no more":   {[]string{"keep"}, "x", command.ErrUsage},
		"two words to run on": {[]string{path, "more"}, "x", command.ErrUsage},
	} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			err := command.Run(t.Context(), row.args, built(), strings.NewReader(row.secret), &out)
			if !errors.Is(err, row.want) {
				t.Errorf("it ended with %v", err)
			}

			if out.Len() != 0 {
				t.Errorf("it said %q", out.String())
			}
		})
	}

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refusing everything, it wrote a store: %v", err)
	}
}

// A secret of the most a secret is, is kept.
func TestTheLargestSecret(t *testing.T) {
	t.Parallel()

	path, dir := configured(t, "kept")

	var out bytes.Buffer

	secret := strings.Repeat("x", 64<<10)
	if err := command.Run(
		t.Context(),
		[]string{"keep", path, "lights", "token"},
		built(),
		strings.NewReader(secret),
		&out,
	); err != nil {
		t.Fatal(err)
	}

	if held, err := store.Dir(dir).For("lights").Get(t.Context(), "token"); err != nil || len(held) != len(secret) {
		t.Errorf("the plugin's store holds %d bytes, %v", len(held), err)
	}
}
