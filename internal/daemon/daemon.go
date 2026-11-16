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
	"slices"
	"time"

	"github.com/duncanite/go-matter"
	"github.com/duncanite/go-matter/bridge"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/plugin"
)

// ErrNoPlugin is a plugin the configuration names that is not compiled
// in.
var ErrNoPlugin = errors.New("no such plugin")

// ErrEnded is a plugin that stopped while the daemon was still running,
// and said nothing of why.
var ErrEnded = errors.New("it ended while the daemon was running")

// invitation is how long the bridge is open to a first controller.
const invitation = 15 * time.Minute

// Run is the daemon: it makes the plugins cfg enables from makers, runs
// them, and serves what they put as a bridge, until ctx ends. It returns
// nil then, and an error when it could not start or a plugin ended.
//
// A bridge no controller holds yet is opened to one, and how to pair
// with it is written to out: the code is what lets a controller in, and
// goes nowhere else.
func Run(ctx context.Context, cfg *config.Config, makers map[string]plugin.Maker, out io.Writer) error {
	plugins, err := made(cfg, makers)
	if err != nil {
		return err
	}

	var home matter.Home

	served, err := bridge.Open(
		ctx,
		bridge.Config{Name: cfg.Bridge.Name, Dir: cfg.Bridge.State, Interface: cfg.Bridge.Interface},
		&home,
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

// made is the plugins the configuration enables, each made from its
// settings.
func made(cfg *config.Config, makers map[string]plugin.Maker) (map[string]plugin.Plugin, error) {
	plugins := map[string]plugin.Plugin{}

	for name, settings := range cfg.Plugins {
		maker, known := makers[name]
		if !known {
			return nil, fmt.Errorf(
				"%w: %q; this build has %v", ErrNoPlugin, name, slices.Sorted(maps.Keys(makers)),
			)
		}

		one, err := maker(plugin.Settings{
			Decode:  config.Settings(&settings),
			Secrets: plugin.Secrets{Dir: cfg.Secrets},
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
