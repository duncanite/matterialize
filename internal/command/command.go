// Package command is what matterialize is asked on its command line: to
// run, or to keep a secret for one of its plugins.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/internal/daemon"
	"github.com/duncanite/matterialize/internal/store"
	"github.com/duncanite/matterialize/plugin"
)

// ErrUsage is matterialize asked wrongly.
var ErrUsage = errors.New("usage: matterialize CONFIG, or: matterialize keep CONFIG PLUGIN NAME < SECRET")

// ErrSecret is a secret that cannot be kept: none, or more than a secret
// is.
var ErrSecret = errors.New("not a secret to keep")

// keep is the word that asks for a secret to be kept.
const keep = "keep"

// most is more than any secret a plugin keeps: a key and its
// certificates are a few thousand bytes.
const most = 64 << 10

// Run does what the arguments ask, the program's name left out.
//
// One argument is the configuration to run the daemon from, as
// daemon.Run does, until ctx ends.
//
// "keep", a configuration, a plugin and a name keep what is read from
// given, whole and as it is, in that plugin's store under that name, in
// place of what was there: what an operator has to give a plugin that
// the plugin cannot get by itself. It is the daemon's own way into its
// store, so that where and how the store is kept stays the daemon's
// alone; a daemon that is running reads what was kept the next time it
// asks. The secret is never written to out.
func Run(ctx context.Context, args []string, makers map[string]plugin.Maker, given io.Reader, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] != keep:
		cfg, err := load(args[0])
		if err != nil {
			return err
		}

		return daemon.Run(ctx, cfg, makers, out)
	case len(args) == 4 && args[0] == keep:
		return kept(ctx, args[1], args[2], args[3], makers, given, out)
	default:
		return ErrUsage
	}
}

func load(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return cfg, nil
}

// kept keeps what given holds for a plugin, under a name.
func kept(
	ctx context.Context, path, owner, name string, makers map[string]plugin.Maker, given io.Reader, out io.Writer,
) error {
	cfg, err := load(path)
	if err != nil {
		return err
	}

	// A plugin this build does not have keeps nothing: a name misspelt
	// would put a secret where nothing ever reads it.
	if _, known := makers[owner]; !known {
		return fmt.Errorf("%w: %q; this build has %v", daemon.ErrNoPlugin, owner, slices.Sorted(maps.Keys(makers)))
	}

	// One byte more than the most, to tell a secret of that size from
	// one cut short.
	secret, err := io.ReadAll(io.LimitReader(given, most+1))
	if err != nil {
		return fmt.Errorf("reading the secret: %w", err)
	}

	switch {
	case len(secret) == 0:
		return fmt.Errorf("%w: nothing was given on standard input", ErrSecret)
	case len(secret) > most:
		return fmt.Errorf("%w: it is more than %d bytes", ErrSecret, most)
	}

	if err = store.Dir(cfg.Store).For(owner).Put(ctx, name, secret); err != nil {
		return fmt.Errorf("keeping %s for the plugin %s: %w", name, owner, err)
	}

	if _, err = fmt.Fprintf(out, "kept %s for the plugin %s\n", name, owner); err != nil {
		return fmt.Errorf("saying so: %w", err)
	}

	return nil
}
