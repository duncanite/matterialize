// Package daemon is matterialize running: the plugins that are enabled
// writing their devices into one home, and that home served as one
// Matter bridge.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"time"

	"github.com/forkcloser/go-upnp/ssdp"

	"github.com/duncanite/go-matter"
	"github.com/duncanite/go-matter/bridge"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/internal/lent"
	"github.com/duncanite/matterialize/internal/selection"
	"github.com/duncanite/matterialize/internal/store"
	"github.com/duncanite/matterialize/plugin"
)

// ErrNoPlugin is a plugin the configuration names that is not compiled
// in.
var ErrNoPlugin = errors.New("no such plugin")

// ErrNoInterface is an interface the configuration names that the host
// does not have.
var ErrNoInterface = errors.New("the host has no such interface")

// ErrEnded is a plugin that stopped while the daemon was still running,
// and said nothing of why.
var ErrEnded = errors.New("it ended while the daemon was running")

// invitation is how long the bridge is open to a first controller.
const invitation = 15 * time.Minute

// Run is the daemon: it makes the plugins cfg enables from makers, runs
// them, and serves what they put, and cfg has served, as a bridge, until
// ctx ends. It returns nil then, and an error when it could not start or
// a plugin ended.
//
// A bridge no controller holds yet is opened to one, and how to pair
// with it is written to out: the code is what lets a controller in, and
// goes nowhere else.
func Run(ctx context.Context, cfg *config.Config, makers map[string]plugin.Maker, out io.Writer) error {
	if err := present(cfg); err != nil {
		return err
	}

	// The host's part in SSDP, for whichever plugins browse, where the
	// configuration has them hear it.
	browsing := lent.NewSSDP(ssdp.Config{}, cfg.Discover.SSDP)
	defer func() { _ = browsing.Close() }()

	// The host's part in Multicast DNS: one, for the bridge and for the
	// plugins alike, under the name Matter has the bridge's host take.
	// It is closed after the bridge is, so that the bridge's goodbyes
	// are said.
	multicast, err := named(ctx, cfg)
	if err != nil {
		return err
	}

	defer func() { _ = multicast.Close() }()

	plugins, err := made(cfg, makers, browsing, multicast)
	if err != nil {
		return err
	}

	var home matter.Home

	served, err := bridge.Open(
		ctx,
		bridge.Config{Name: cfg.Bridge.Name, Dir: cfg.Bridge.State, Network: multicast},
		selection.Of(&home, cfg.Serve),
	)
	if err != nil {
		return fmt.Errorf("opening the bridge: %w", err)
	}

	defer func() { _ = served.Close() }()

	if err = invite(ctx, served, out); err != nil {
		return err
	}

	// One plugin ending is the end of them all: a daemon that serves half of a
	// home and says nothing is worse than one that stops and is started
	// again.
	running, stop := context.WithCancel(ctx)
	defer stop()

	ended := make(chan error, len(plugins))

	for _, name := range slices.Sorted(maps.Keys(plugins)) {
		go func() {
			lost := plugins[name].Run(running, &home)

			switch {
			case lost != nil:
				ended <- fmt.Errorf("the plugin %s: %w", name, lost)
			case running.Err() == nil:
				// Run is to last as long as the daemon does.
				ended <- fmt.Errorf("the plugin %s: %w", name, ErrEnded)
			default:
				ended <- nil
			}
		}()
	}

	var failed error

	for range plugins {
		if err = <-ended; failed == nil {
			failed = err
		}

		stop()
	}

	return failed
}

// named puts the daemon on Multicast DNS under the name its host has: a
// host with no hardware address to be named by is refused, since a
// bridge is announced under no other name.
func named(ctx context.Context, cfg *config.Config) (*lent.MDNS, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("reading the host's interfaces: %w", err)
	}

	host, err := lent.HostOf(interfaces, cfg.Bridge.Interface)
	if err != nil {
		return nil, fmt.Errorf("naming the host: %w", err)
	}

	return lent.OpenMDNS(ctx, lent.MDNSConfig{Host: host, Bridge: cfg.Bridge.Interface, Plugins: cfg.Discover.MDNS})
}

// present says whether the host has every interface the configuration
// names, when the daemon starts. One it has not is refused, by its name:
// a name misspelt would otherwise listen nowhere and say nothing, and
// SSDP takes up no interface that comes after it began.
func present(cfg *config.Config) error {
	named := slices.Concat(cfg.Discover.SSDP, cfg.Discover.MDNS)
	if cfg.Bridge.Interface != "" {
		named = append(named, cfg.Bridge.Interface)
	}

	for _, name := range named {
		if _, err := net.InterfaceByName(name); err != nil {
			return fmt.Errorf("%w: %q", ErrNoInterface, name)
		}
	}

	return nil
}

// made is the plugins the configuration enables, each made from its
// settings and what the daemon lends it.
func made(
	cfg *config.Config, makers map[string]plugin.Maker, browsing plugin.SSDP, multicast plugin.MDNS,
) (map[string]plugin.Plugin, error) {
	plugins := map[string]plugin.Plugin{}

	for name, settings := range cfg.Plugins {
		maker, known := makers[name]
		if !known {
			return nil, fmt.Errorf(
				"%w: %q; this build has %v", ErrNoPlugin, name, slices.Sorted(maps.Keys(makers)),
			)
		}

		one, err := maker(plugin.Settings{
			Decode: config.Settings(&settings),
			Store:  store.Dir(cfg.Store).For(name),
			SSDP:   browsing,
			MDNS:   multicast,
		})
		if err != nil {
			return nil, fmt.Errorf("the plugin %s: %w", name, err)
		}

		plugins[name] = one
	}

	return plugins, nil
}

// invite opens a bridge no controller holds to a first one, and writes
// how to pair with it.
func invite(ctx context.Context, served *bridge.Bridge, out io.Writer) error {
	held, err := served.Fabrics(ctx)
	if err != nil {
		return fmt.Errorf("asking who holds the bridge: %w", err)
	}

	if len(held) > 0 {
		return nil
	}

	code, err := served.Invite(ctx, invitation)
	if err != nil {
		return fmt.Errorf("opening the bridge to a controller: %w", err)
	}

	_, err = fmt.Fprintf(
		out,
		"No controller holds this bridge yet. For the next %s, pair with:\n  manual code: %s\n  QR text:     %s\n",
		invitation, code.Manual, code.QR,
	)
	if err != nil {
		return fmt.Errorf("writing how to pair: %w", err)
	}

	return nil
}
