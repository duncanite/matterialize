// Package plugin is what a maker's devices are to matterialize: a Plugin
// writes them into the home the bridge serves, and keeps them there as
// the maker says they are.
//
// A plugin is compiled in and enabled by configuration: the daemon makes
// one for each name its configuration has, with the settings under that
// name, and runs it for as long as the daemon runs.
//
// A plugin has no file of its own and reads none: what it has to keep
// from one run to the next, it gives the Store it is lent.
package plugin

import (
	"context"
	"errors"

	"github.com/duncanite/go-matter"
)

// ErrSettings is settings a plugin cannot be made from.
var ErrSettings = errors.New("the plugin's settings are not valid")

// ErrNotStored is a name a Store holds nothing under.
var ErrNotStored = errors.New("nothing is stored under that name")

// Plugin is one maker's devices, written into a home.
type Plugin interface {
	// Run puts the maker's devices into home and keeps them as the maker
	// says they are: their state reported as it changes, a device that
	// went or came put or removed. A maker that cannot be reached for a
	// while is Run's to wait for and reach again, its devices reported
	// unreachable meanwhile. It returns nil when ctx ends, having let go
	// of the maker; and an error only for what it cannot come back
	// from, which ends the daemon.
	Run(ctx context.Context, home *matter.Home) error
}

// Maker makes a Plugin from what the configuration has under its name.
// It reads its settings and reaches for nothing, on the network or in
// the store: that is Run's.
type Maker func(settings Settings) (Plugin, error)

// Settings is what a plugin is made from.
type Settings struct {
	// Decode fills into, a pointer to a struct, from what the
	// configuration has under the plugin's name. A key the struct has no
	// field for is an error, as is a value of the wrong kind.
	Decode func(into any) error
	// Store keeps what the plugin gives it, for this plugin alone.
	Store Store
}

// Store keeps what a plugin cannot ask its maker for again: the
// credentials a pairing gave, a token. What it holds is secret, and is
// there after a restart. Where and how it is kept is the daemon's to
// say, and nothing a plugin knows.
//
// A name is one plain word of the plugin's choosing: letters, digits,
// "-" and "_". A Store may be used from several goroutines.
type Store interface {
	// Get is what was last put under name, and an ErrNotStored when
	// nothing was.
	Get(ctx context.Context, name string) ([]byte, error)
	// Put keeps value under name, in place of what was there: whole, or
	// not at all.
	Put(ctx context.Context, name string, value []byte) error
}
