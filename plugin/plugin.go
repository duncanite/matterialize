// Package plugin is what a maker's devices are to matterialize: a Plugin
// writes them into the home the bridge serves, and keeps them there as
// the maker says they are.
//
// A plugin is compiled in and enabled by configuration: the daemon makes
// one for each name its configuration has, with the settings under that
// name, and runs it for as long as the daemon runs.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mycophonic/primordium/filesystem/pathcheck"

	"github.com/duncanite/go-matter"
)

// ErrSettings is settings a plugin cannot be made from.
var ErrSettings = errors.New("the plugin's settings are not valid")

// Plugin is one maker's devices, written into a home.
type Plugin interface {
	// Run puts the maker's devices into home and keeps them as the maker
	// says they are: their state reported as it changes, a device that
	// went or came put or removed. It returns nil when ctx ends, having
	// let go of the maker; and an error when the maker is lost for good,
	// which ends the daemon.
	Run(ctx context.Context, home *matter.Home) error
}

// Maker makes a Plugin from what the configuration has under its name.
// It reads its settings and its secrets and reaches for nothing on the
// network: that is Run's.
type Maker func(settings Settings) (Plugin, error)

// Settings is what a plugin is made from.
type Settings struct {
	// Decode fills into, a pointer to a struct, from what the
	// configuration has under the plugin's name. A key the struct has no
	// field for is an error, as is a value of the wrong kind.
	Decode func(into any) error
	// Secrets is where what is not written in the configuration is read.
	Secrets Secrets
}

// Secrets is what a plugin needs that the configuration does not hold:
// key material in files, passwords in the environment.
type Secrets struct {
	// Dir is the directory the daemon was given for key material. Empty
	// when it was given none.
	Dir string
}

// Path is a file or a directory of key material, by its name in the
// directory of secrets: one plain name, never a path out of it.
func (s Secrets) Path(name string) (string, error) {
	if s.Dir == "" {
		return "", fmt.Errorf("%w: no directory of secrets was given", ErrSettings)
	}

	if err := pathcheck.ValidateComponent(name); err != nil {
		return "", fmt.Errorf("%w: the secret %q: %w", ErrSettings, name, err)
	}

	return filepath.Join(s.Dir, name), nil
}

// Env is a secret the environment holds, by the variable's name. It is
// an error for the variable to be unset or empty: a plugin that asks for
// one needs it.
func (Secrets) Env(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%w: the environment has no %s", ErrSettings, name)
	}

	return value, nil
}
