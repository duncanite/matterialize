package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/duncanite/matterialize/internal/config"
)

const whole = `
bridge:
  name: Home
  state: /var/lib/matterialize
  interface: en0
store: /var/lib/matterialize/store
plugins:
  lights:
    host: 192.0.2.20
    rooms: [Kitchen, Hall]
  plugs:
`

// A configuration is read as it is written: the bridge, the directory
// of what the plugins keep, and each plugin that is enabled with its own
// settings.
func TestAConfigurationIsRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
		t.Fatal(err)
	}

	read, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if read.Bridge != (config.Bridge{Name: "Home", State: "/var/lib/matterialize", Interface: "en0"}) ||
		read.Store != "/var/lib/matterialize/store" || len(read.Plugins) != 2 {
		t.Errorf("read %+v", read)
	}

	var lights struct {
		Host  string   `yaml:"host"`
		Rooms []string `yaml:"rooms"`
	}

	node := read.Plugins["lights"]
	if err = config.Settings(&node)(&lights); err != nil || lights.Host != "192.0.2.20" || len(lights.Rooms) != 2 {
		t.Errorf("the settings of lights: %+v, %v", lights, err)
	}

	// A key the plugin has no field for is refused.
	var narrow struct {
		Host string `yaml:"host"`
	}

	if err = config.Settings(
		&node,
	)(
		&narrow,
	); !errors.Is(err, config.ErrInvalid) ||
		!strings.Contains(err.Error(), "rooms") {
		t.Errorf("settings with a key the plugin does not know: %v", err)
	}

	// A plugin enabled with nothing under its name has no settings, and
	// that is not an error.
	var none struct {
		Host string `yaml:"host"`
	}

	node = read.Plugins["plugs"]
	if err = config.Settings(&node)(&none); err != nil || none.Host != "" {
		t.Errorf("the settings of a plugin with none: %+v, %v", none, err)
	}

	if _, err = config.Load(filepath.Join(t.TempDir(), "none.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a configuration that is not there: %v", err)
	}
}

// What cannot be a configuration is refused, with what is wrong.
func TestWhatIsNoConfiguration(t *testing.T) {
	t.Parallel()

	for what, row := range map[string]struct{ written, says string }{
		"a key that is none": {
			"bridge: {name: Home, state: /s}\nplugins: {lights: }\nplugin: {}\n", "plugin",
		},
		"a key of the bridge that is none": {
			"bridge: {name: Home, state: /s, port: 5540}\nplugins: {lights: }\n", "port",
		},
		"no name":     {"bridge: {state: /s}\nplugins: {lights: }\n", "the bridge has no name"},
		"no state":    {"bridge: {name: Home}\nplugins: {lights: }\n", "the bridge has no state directory"},
		"no plugin":   {"bridge: {name: Home, state: /s}\n", "no plugin is enabled"},
		"not YAML":    {"bridge: [", "the configuration is not valid"},
		"nothing":     {"", "the configuration is not valid"},
		"another doc": {"- one\n- two\n", "the configuration is not valid"},
	} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()

			if _, err := config.Parse([]byte(row.written)); !errors.Is(err, config.ErrInvalid) ||
				!strings.Contains(err.Error(), row.says) {
				t.Errorf("refused with %v", err)
			}
		})
	}
}

// What the bridge serves is read as it is written, is every device when
// it is left out, and is refused when it names nothing.
func TestWhatIsServed(t *testing.T) {
	t.Parallel()

	const rest = "bridge: {name: Home, state: /state}\nplugins: {lights: {}}\n"

	read, err := config.Parse([]byte(rest + "serve: [lights/*, plugs/one]\n"))
	if err != nil || len(read.Serve) != 2 || read.Serve[0] != "lights/*" || read.Serve[1] != "plugs/one" {
		t.Fatalf("read %+v, %v", read, err)
	}

	if read, err = config.Parse([]byte(rest)); err != nil || read.Serve != nil {
		t.Fatalf("with nothing said of what is served, read %+v, %v", read, err)
	}

	// A key with nothing under it is not a key left out: it would serve
	// every device where the last pattern was commented out.
	for _, written := range []string{
		"serve: []\n", "serve: ['']\n", "serve: ['a*b']\n",
		"serve:\n", "serve:\n  # - lights/*\n", "serve: ~\n", "serve: null\n", "serve: lights/*\n",
	} {
		if _, err = config.Parse([]byte(rest + written)); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("%q: %v", written, err)
		}
	}
}

// Where the daemon listens for devices is read as it is written, is
// every interface for a means that is left out, and is refused when it
// names none.
func TestWhereDevicesAreHeard(t *testing.T) {
	t.Parallel()

	const rest = "bridge: {name: Home, state: /state}\nplugins: {lights: {}}\n"

	read, err := config.Parse([]byte(rest + "discover:\n  ssdp: [eth0]\n  mdns: [eth0, eth1]\n"))
	if err != nil || !slices.Equal(read.Discover.SSDP, []string{"eth0"}) ||
		!slices.Equal(read.Discover.MDNS, []string{"eth0", "eth1"}) {
		t.Fatalf("read %+v, %v", read, err)
	}

	for _, written := range []string{"", "discover:\n", "discover: {}\n"} {
		if read, err = config.Parse([]byte(rest + written)); err != nil || read.Discover.SSDP != nil ||
			read.Discover.MDNS != nil {
			t.Errorf("%q: read %+v, %v", written, read, err)
		}
	}

	if read, err = config.Parse([]byte(rest + "discover: {mdns: [eth1]}\n")); err != nil ||
		read.Discover.SSDP != nil || len(read.Discover.MDNS) != 1 {
		t.Errorf("with one means named, read %+v, %v", read, err)
	}

	// A key with nothing under it is not a key left out: it would listen
	// everywhere where the last interface was commented out.
	for _, written := range []string{
		"discover: {ssdp: []}\n", "discover: {mdns: []}\n", "discover:\n  ssdp:\n", "discover:\n  mdns:\n  # - eth0\n",
		"discover: {mdns: ~}\n", "discover: {ssdp: ['']}\n", "discover: {mdns: eth0}\n", "discover: {upnp: [eth0]}\n",
		"discover: [eth0]\n",
	} {
		if _, err = config.Parse([]byte(rest + written)); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("%q: %v", written, err)
		}
	}
}
