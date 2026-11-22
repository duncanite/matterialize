// Package store is where the daemon keeps what its plugins give it to
// keep: files under one directory, each plugin's in a directory of its
// own, that the daemon's user alone can read.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/mycophonic/primordium/filesystem"

	"github.com/duncanite/matterialize/plugin"
)

// ErrNone is a store asked for when the configuration names none.
var ErrNone = errors.New("the configuration names no store")

// ErrName is a name that is not one plain word.
var ErrName = errors.New("a name is letters, digits, - and _")

const (
	private     = 0o600
	privateDir  = 0o700
	longestName = 64
)

// plain is what a name is made of: nothing a path is made with.
var plain = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Dir is a store in a directory. The empty one is no store: everything
// asked of it is refused with ErrNone.
type Dir string

// For is the part of the store that is one plugin's, by the plugin's
// name.
func (d Dir) For(name string) plugin.Store {
	return kept{root: string(d), owner: name}
}

// kept is one plugin's part of a Dir.
type kept struct {
	root  string
	owner string
}

// Get is plugin.Store's.
func (k kept) Get(_ context.Context, name string) ([]byte, error) {
	path, err := k.path(name)
	if err != nil {
		return nil, err
	}

	value, err := os.ReadFile(path) // #nosec G304 -- under the store, by a name that is one plain word
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s of %s", plugin.ErrNotStored, name, k.owner)
	}

	if err != nil {
		return nil, fmt.Errorf("reading %s of %s from the store: %w", name, k.owner, err)
	}

	return value, nil
}

// Put is plugin.Store's.
func (k kept) Put(_ context.Context, name string, value []byte) error {
	path, err := k.path(name)
	if err != nil {
		return err
	}

	if err = os.MkdirAll(filepath.Dir(path), privateDir); err != nil {
		return fmt.Errorf("making the store of %s: %w", k.owner, err)
	}

	if err = filesystem.WriteFile(path, value, private); err != nil {
		return fmt.Errorf("keeping %s of %s in the store: %w", name, k.owner, err)
	}

	return nil
}

// path is the file a name is kept in.
func (k kept) path(name string) (string, error) {
	if k.root == "" {
		return "", ErrNone
	}

	for _, word := range []string{k.owner, name} {
		if len(word) > longestName || !plain.MatchString(word) {
			return "", fmt.Errorf("%w: not %q", ErrName, word)
		}
	}

	return filepath.Join(k.root, k.owner, name), nil
}
