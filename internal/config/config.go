// Package config reads what matterialize is told to be: the bridge it
// serves, and the plugins that are enabled with their settings.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"

	"go.yaml.in/yaml/v4"

	"github.com/duncanite/matterialize/internal/selection"
)

// ErrInvalid is a configuration that cannot be used.
var ErrInvalid = errors.New("the configuration is not valid")

// Config is a configuration, as it is written.
type Config struct {
	// Bridge is the Matter bridge the daemon is.
	Bridge Bridge `yaml:"bridge"`
	// Store is the directory the daemon keeps in what its plugins give
	// it to keep: the credentials a pairing gave. It is secret. It may
	// be left out when no plugin that is enabled keeps anything.
	Store string `yaml:"store"`
	// Serve is which devices the bridge serves, of those the plugins
	// find: each a device's ID, or the beginning of one and "*". With
	// the key left out, it is nil, and every device is served.
	Serve []string `yaml:"-"`
	// Discover is where the daemon listens for devices, on its plugins'
	// behalf.
	Discover Discover `yaml:"-"`
	// Plugins is the plugins that are enabled, by name, each with its
	// settings as that plugin reads them. A plugin that is not named
	// does not run.
	Plugins map[string]yaml.Node `yaml:"plugins"`
}

// Discover is the interfaces the daemon listens on for the devices its
// plugins look for, by the means they are found by. A plugin chooses
// none: it is told of what is heard where these say. Each is a list of
// interface names, as the host has them; with a key left out it is nil,
// and that means is listened for on every interface.
type Discover struct {
	// SSDP is where UPnP devices are heard.
	SSDP []string
	// MDNS is where what is announced over Multicast DNS is heard.
	MDNS []string
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
	// A list is read as a node: a key that is there with nothing under
	// it, its last item commented out, is not a key left out, and a list
	// of strings cannot tell the two apart.
	var whole struct {
		Config `yaml:",inline"`

		Serve    yaml.Node `yaml:"serve"`
		Discover listed    `yaml:"discover"`
	}

	decoder := yaml.NewDecoder(bytes.NewReader(written))
	decoder.KnownFields(true)

	if err := decoder.Decode(&whole); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	read := whole.Config

	for _, one := range []struct {
		key  string
		node *yaml.Node
		into *[]string
	}{
		{"serve", &whole.Serve, &read.Serve},
		{"discover.ssdp", &whole.Discover.SSDP, &read.Discover.SSDP},
		{"discover.mdns", &whole.Discover.MDNS, &read.Discover.MDNS},
	} {
		words, err := list(one.node)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, one.key, err)
		}

		*one.into = words
	}

	switch {
	case read.Bridge.Name == "":
		return nil, fmt.Errorf("%w: the bridge has no name", ErrInvalid)
	case read.Bridge.State == "":
		return nil, fmt.Errorf("%w: the bridge has no state directory", ErrInvalid)
	case len(read.Plugins) == 0:
		return nil, fmt.Errorf("%w: no plugin is enabled", ErrInvalid)
	}

	if err := selection.Check(read.Serve); err != nil {
		return nil, fmt.Errorf("%w: serve: %w", ErrInvalid, err)
	}

	for key, names := range map[string][]string{
		"discover.ssdp": read.Discover.SSDP, "discover.mdns": read.Discover.MDNS,
	} {
		switch {
		case names != nil && len(names) == 0:
			return nil, fmt.Errorf("%w: %s: the list holds none, and would listen nowhere", ErrInvalid, key)
		case slices.Contains(names, ""):
			return nil, fmt.Errorf("%w: %s: an interface with no name", ErrInvalid, key)
		}
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

// listed is Discover as it is written: its lists as nodes.
type listed struct {
	SSDP yaml.Node `yaml:"ssdp"`
	MDNS yaml.Node `yaml:"mdns"`
}

// list is a list of words as it is written under a key: nil for a key
// that is not there, and never nil for one that is, whatever is under
// it.
func list(node *yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}

	words := []string{}

	if node.Tag == "!!null" {
		return words, nil
	}

	if err := node.Decode(&words); err != nil {
		return nil, fmt.Errorf("not a list: %w", err)
	}

	return words, nil
}
