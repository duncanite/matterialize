// Package config reads what matterialize is told to be: the bridge it
// serves, and the plugins that are enabled with their settings.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"go.yaml.in/yaml/v4"
)

// ErrInvalid is a configuration that cannot be used.
var ErrInvalid = errors.New("the configuration is not valid")

// Config is a configuration, as it is written.
type Config struct {
	// Bridge is the Matter bridge the daemon is.
	Bridge Bridge `yaml:"bridge"`
	// Secrets is a directory of key material for the plugins. It may be
	// left out when no plugin that is enabled reads one.
	Secrets string `yaml:"secrets"`
	// Plugins is the plugins that are enabled, by name, each with its
	// settings as that plugin reads them. A plugin that is not named
	// does not run.
	Plugins map[string]yaml.Node `yaml:"plugins"`
}

// Bridge is the bridge as a controller sees it.
type Bridge struct {
	// Name is what a controller shows for the bridge.
	Name string `yaml:"name"`
	// State is the directory the bridge keeps as its own: who paired
	// with it, and which device has which place. The same directory is
	// the same bridge after a restart; without it every controller has
	// to be paired again.
	State string `yaml:"state"`
	// Interface is the network interface the bridge announces itself
	// on. Left out, it is all of them.
	Interface string `yaml:"interface"`
}

// Load reads the configuration in a file.
func Load(path string) (*Config, error) {
	written, err := os.ReadFile(path) // #nosec G304 -- the configuration is the file the operator names
	if err != nil {
		return nil, fmt.Errorf("reading the configuration: %w", err)
	}

	return Parse(written)
}

// Parse reads a configuration. A key it does not know is an error, with
// where it is: a misspelt key that silently enables nothing is worse
// than a refusal.
func Parse(written []byte) (*Config, error) {
	var read Config

	decoder := yaml.NewDecoder(bytes.NewReader(written))
	decoder.KnownFields(true)

	if err := decoder.Decode(&read); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	switch {
	case read.Bridge.Name == "":
		return nil, fmt.Errorf("%w: the bridge has no name", ErrInvalid)
	case read.Bridge.State == "":
		return nil, fmt.Errorf("%w: the bridge has no state directory", ErrInvalid)
	case len(read.Plugins) == 0:
		return nil, fmt.Errorf("%w: no plugin is enabled", ErrInvalid)
	}

	return &read, nil
}

// Settings is a function that fills a struct from a plugin's settings,
// refusing a key the struct has no field for.
func Settings(node *yaml.Node) func(into any) error {
	return func(into any) error {
		// A plugin enabled with nothing under its name has no settings.
		if node.Kind == 0 || node.Tag == "!!null" {
			return nil
		}

		written, err := yaml.Marshal(node)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		decoder := yaml.NewDecoder(bytes.NewReader(written))
		decoder.KnownFields(true)

		if err = decoder.Decode(into); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		return nil
	}
}
