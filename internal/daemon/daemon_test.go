package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-matter"
	"github.com/duncanite/go-matter/mattertest"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/internal/daemon"
	"github.com/duncanite/matterialize/plugin"
)

// errLost is a maker lost for good.
var errLost = errors.New("the maker is lost")

// lights is a plugin of a test: it puts one light, named as its settings
// have it, and then does as the test has it do.
type lights struct {
	Name string `yaml:"name"`

	// put is closed once the light is in the home; ends is why Run ends
	// before the daemon does, when it does; let is closed to let it.
	put  chan struct{}
	ends error
	let  chan struct{}
	// left is closed when Run returns.
	left chan struct{}
}

func (l *lights) Run(ctx context.Context, home *matter.Home) error {
	defer close(l.left)

	mattertest.Light(home, matter.ID(l.Name))
	close(l.put)

	select {
	case <-ctx.Done():
		return nil
	case <-l.let:
		return l.ends
	}
}

// buffer is what the daemon wrote, safe to read while it writes.
type buffer struct {
	mu      sync.Mutex
	written bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.written.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.written.String()
}

// configured is a configuration that enables the plugins named.
func configured(t *testing.T, plugins string) *config.Config {
	t.Helper()

	read, err := config.Parse([]byte(
		"bridge: {name: Test bridge, state: " + filepath.Join(t.TempDir(), "state") + "}\n" +
			"store: " + filepath.Join(t.TempDir(), "store") + "\nplugins:\n" + plugins,
	))
	if err != nil {
		t.Fatal(err)
	}

	return read
}

// made is a plugin of a test and the maker that gives it to the daemon,
// filled from the settings the daemon hands it.
func made(ends error) (*lights, plugin.Maker, *plugin.Settings) {
	one := &lights{ends: ends, put: make(chan struct{}), let: make(chan struct{}), left: make(chan struct{})}
	given := &plugin.Settings{}

	return one, func(settings plugin.Settings) (plugin.Plugin, error) {
		*given = settings

		return one, settings.Decode(one)
	}, given
}

// The daemon makes each plugin its configuration enables from the
// settings under its name, runs it, serves what it puts as a bridge a
// controller can pair with, and stops with its context.
func TestTheDaemonRunsWhatIsEnabled(t *testing.T) {
	t.Parallel()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	kitchen, maker, given := made(nil)
	out := &buffer{}
	ended := make(chan error, 1)

	go func() {
		ended <- daemon.Run(
			ctx,
			configured(t, "  lights: {name: Kitchen}\n"),
			map[string]plugin.Maker{"lights": maker, "unused": nil},
			out,
		)
	}()

	select {
	case <-kitchen.put:
	case err := <-ended:
		t.Fatalf("the daemon ended with %v", err)
	}

	if kitchen.Name != "Kitchen" {
		t.Errorf("the plugin was made with the name %q", kitchen.Name)
	}

	// It is lent the daemon's means of finding devices.
	if given.SSDP == nil || given.MDNS == nil {
		t.Errorf("the plugin was lent SSDP %v and Multicast DNS %v", given.SSDP, given.MDNS)
	}

	// The store it was made with keeps what it is given.
	if err := given.Store.Put(ctx, "token", []byte("kept")); err != nil {
		t.Errorf("keeping a token: %v", err)
	}

	if kept, err := given.Store.Get(ctx, "token"); err != nil || string(kept) != "kept" {
		t.Errorf("the token kept is %q, %v", kept, err)
	}

	// No controller holds a bridge that is new: it says how to pair.
	if said := out.String(); !strings.Contains(said, "manual code: ") || !strings.Contains(said, "QR text:     MT:") {
		t.Errorf("wrote %q", said)
	}

	stop()

	select {
	case err := <-ended:
		if err != nil {
			t.Errorf("stopped, the daemon ended with %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the daemon did not stop")
	}

	<-kitchen.left
}

// A plugin ending is the end of the daemon, with why, and the other plugins
// are let go of: half a home is not served in silence.
func TestAPluginThatEnds(t *testing.T) {
	t.Parallel()

	for what, row := range map[string]struct {
		ends error
		want error
	}{
		"lost for good":       {ends: errLost, want: errLost},
		"with nothing to say": {ends: nil, want: daemon.ErrEnded},
	} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()

			failing, first, _ := made(row.ends)
			staying, second, _ := made(nil)
			ended := make(chan error, 1)

			go func() {
				ended <- daemon.Run(
					t.Context(),
					configured(t, "  failing: {name: One}\n  staying: {name: Two}\n"),
					map[string]plugin.Maker{"failing": first, "staying": second},
					&buffer{},
				)
			}()

			<-failing.put
			<-staying.put
			close(failing.let)

			select {
			case err := <-ended:
				if !errors.Is(err, row.want) || !strings.Contains(err.Error(), "the plugin failing") {
					t.Errorf("the daemon ended with %v", err)
				}
			case <-time.After(time.Minute):
				t.Fatal("the daemon did not end")
			}

			<-staying.left
		})
	}
}

// An interface the configuration names that the host does not have is
// refused by its name, wherever it is named, before anything is opened:
// misspelt, it would listen nowhere and say nothing.
func TestAnInterfaceTheHostHasNot(t *testing.T) {
	t.Parallel()

	_, maker, _ := made(nil)

	for where, name := range map[string]func(*config.Config){
		"the bridge's": func(cfg *config.Config) { cfg.Bridge.Interface = "nowhere0" },
		"one for SSDP": func(cfg *config.Config) { cfg.Discover.SSDP = []string{"nowhere0"} },
		"one for mDNS": func(cfg *config.Config) { cfg.Discover.MDNS = []string{"nowhere0"} },
	} {
		cfg := configured(t, "  lights: {name: Kitchen}\n")
		name(cfg)

		err := daemon.Run(t.Context(), cfg, map[string]plugin.Maker{"lights": maker}, &buffer{})
		if !errors.Is(err, daemon.ErrNoInterface) || !strings.Contains(err.Error(), `"nowhere0"`) {
			t.Errorf("%s: %v", where, err)
		}
	}
}

// What cannot be started is refused before anything is served: a plugin
// this build does not have, and one whose settings it cannot be made
// from.
func TestWhatTheDaemonRefuses(t *testing.T) {
	t.Parallel()

	_, maker, _ := made(nil)

	err := daemon.Run(
		t.Context(),
		configured(t, "  blinds:\n"),
		map[string]plugin.Maker{"lights": maker, "plugs": maker},
		&buffer{},
	)
	if !errors.Is(err, daemon.ErrNoPlugin) ||
		!strings.Contains(err.Error(), `"blinds"; this build has [lights plugs]`) {
		t.Errorf("a plugin this build does not have: %v", err)
	}

	err = daemon.Run(
		t.Context(),
		configured(t, "  lights: {colour: red}\n"),
		map[string]plugin.Maker{"lights": maker},
		&buffer{},
	)
	if !errors.Is(err, config.ErrInvalid) || !strings.Contains(err.Error(), "the plugin lights") {
		t.Errorf("settings the plugin cannot be made from: %v", err)
	}
}
