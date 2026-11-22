package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/duncanite/matterialize/internal/store"
	"github.com/duncanite/matterialize/plugin"
)

// What a plugin puts is what it gets, the last of it, after the store
// is opened again too; and is nothing another plugin sees.
func TestWhatIsPutIsKept(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "store")
	lights := store.Dir(dir).For("lights")
	ctx := t.Context()

	if got, err := lights.Get(ctx, "token"); !errors.Is(err, plugin.ErrNotStored) {
		t.Errorf("before anything is put: %q, %v", got, err)
	}

	for _, value := range []string{"first", "", "last"} {
		if err := lights.Put(ctx, "token", []byte(value)); err != nil {
			t.Fatal(err)
		}

		if got, err := store.Dir(dir).For("lights").Get(ctx, "token"); err != nil || string(got) != value {
			t.Errorf("after %q was put: %q, %v", value, got, err)
		}
	}

	if got, err := store.Dir(dir).For("plugs").Get(ctx, "token"); !errors.Is(err, plugin.ErrNotStored) {
		t.Errorf("another plugin sees %q, %v", got, err)
	}
}

// What is kept is the daemon's user's alone to read.
func TestWhatIsKeptIsPrivate(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("a mode says nothing of who reads a file on Windows")
	}

	dir := filepath.Join(t.TempDir(), "store")
	if err := store.Dir(dir).For("lights").Put(t.Context(), "token", []byte("kept")); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "lights"):          0o700,
		filepath.Join(dir, "lights", "token"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		if info.Mode().Perm() != want {
			t.Errorf("%s: %v", path, info.Mode().Perm())
		}
	}
}

// A name is one plain word, the plugin's as the value's: nothing that
// could be a path out of the plugin's own part of the store.
func TestANameIsOnePlainWord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := t.Context()

	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "../lights", "a b", ".hidden", "-a", strings.Repeat("a", 65)} {
		if err := store.Dir(dir).For("lights").Put(ctx, name, nil); !errors.Is(err, store.ErrName) {
			t.Errorf("putting under %q: %v", name, err)
		}

		if _, err := store.Dir(dir).For("lights").Get(ctx, name); !errors.Is(err, store.ErrName) {
			t.Errorf("getting %q: %v", name, err)
		}

		if err := store.Dir(dir).For(name).Put(ctx, "token", nil); !errors.Is(err, store.ErrName) {
			t.Errorf("putting for the plugin %q: %v", name, err)
		}
	}

	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the store holds %v, %v", entries, err)
	}
}

// A configuration that names no store has none: nothing is kept, and
// nothing is said to be.
func TestNoStore(t *testing.T) {
	t.Parallel()

	none := store.Dir("").For("lights")

	if err := none.Put(t.Context(), "token", []byte("kept")); !errors.Is(err, store.ErrNone) {
		t.Errorf("putting: %v", err)
	}

	if got, err := none.Get(t.Context(), "token"); !errors.Is(err, store.ErrNone) {
		t.Errorf("getting: %q, %v", got, err)
	}
}

// What cannot be read or written is said, and is not "nothing stored".
func TestAStoreThatFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := t.Context()

	// A file where the plugin's directory would be.
	if err := os.WriteFile(filepath.Join(dir, "lights"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	lights := store.Dir(dir).For("lights")

	if err := lights.Put(ctx, "token", nil); err == nil {
		t.Error("putting under a file was taken")
	}

	// A directory where a value would be.
	if err := os.MkdirAll(filepath.Join(dir, "plugs", "token"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got, err := store.Dir(dir).For("plugs").Get(ctx, "token"); err == nil || errors.Is(err, plugin.ErrNotStored) {
		t.Errorf("getting a directory: %q, %v", got, err)
	}
}
