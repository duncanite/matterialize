// Package wemo is the plugin for Belkin's Wi-Fi Wemo plugs: every plug
// that advertises itself on the local network, written into the home as
// a plug and kept as it says it is.
//
// Its settings, every one of which may be left out:
//
//	wemo:
//	  # How long a plug is given to answer, and how long before one that
//	  # did not is reached for again. Left out, ten seconds.
//	  retry: 10s
//	  # The port plugs send their events to. Left out, any free one.
//	  port: 49200
//
// The plugin finds the plugs through the SSDP the daemon lends it, and
// keeps nothing. A plug tells what it is at by calling this host back,
// at the host's address on the plug's own network and the port above:
// a plug that cannot reach it there is never put into the home.
//
// A plug has no credential, and neither has what it says: whatever
// advertises itself as a plug, and describes itself as that plug where
// it says it is, is taken for one.
package wemo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/forkcloser/go-upnp/ssdp"

	"github.com/duncanite/go-matter"
	"github.com/duncanite/go-wemo/wemo"

	"github.com/duncanite/matterialize/plugin"
)

// again is how long a plug is given to answer, and how long the plugin
// waits before it reaches again for one that did not, when its settings
// do not say.
const again = 10 * time.Second

var (
	// errNowhere is a plug that advertises itself and says no place it
	// is at.
	errNowhere = errors.New("it says nowhere it is at")
	// errNoWayBack is a plug heard on a network this host has no address
	// on: nothing it could call back.
	errNoWayBack = errors.New("this host has no address on its network for it to call back")
	// errAnother is what answers where a plug says it is, and says it is
	// another.
	errAnother = errors.New("what answers there says it is another plug")
	// errSilent is a plug that took a subscription and said nothing: it
	// cannot reach the address it was given.
	errSilent = errors.New("it said nothing of how it stands: it cannot call this host back")
)

// settings is the plugin's, as the configuration writes them.
type settings struct {
	Retry string `yaml:"retry"`
	Port  uint16 `yaml:"port"`
}

// served is the plugin, made.
type served struct {
	ssdp   plugin.SSDP
	retry  time.Duration
	port   uint16
	client *http.Client

	mu sync.Mutex
	// plugs is each plug as it was last opened, which a device's
	// commands go to: where a plug is changes, and the device stays.
	plugs map[wemo.ID]*wemo.Plug
	// listeners is where plugs send their events, one for each of this
	// host's addresses a plug was heard on.
	listeners map[netip.Addr]*wemo.Events
}

// followed is one plug being kept in the home.
type followed struct {
	stop context.CancelFunc
	done chan struct{}
}

// New makes the plugin from its settings: it is the Maker to enable
// under the name "wemo".
func New(given plugin.Settings) (plugin.Plugin, error) {
	var read settings

	if err := given.Decode(&read); err != nil {
		return nil, fmt.Errorf("its settings: %w", err)
	}

	retry := again

	if read.Retry != "" {
		var err error

		if retry, err = time.ParseDuration(read.Retry); err != nil || retry <= 0 {
			return nil, fmt.Errorf("%w: retry is a length of time, as 10s is, not %q", plugin.ErrSettings, read.Retry)
		}
	}

	return &served{
		ssdp:  given.SSDP,
		retry: retry,
		port:  read.Port,
		client: &http.Client{
			Timeout: again,
			// A client that follows redirects sends a request wherever a
			// plug points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		plugs:     map[wemo.ID]*wemo.Plug{},
		listeners: map[netip.Addr]*wemo.Events{},
	}, nil
}

// Run is Plugin's. It ends with ctx, and sooner only when the network
// can be browsed no more: a plug that cannot be reached, or that goes,
// is unreachable in the home until it is there again.
func (s *served) Run(ctx context.Context, home *matter.Home) error {
	running, stop := context.WithCancel(ctx)
	following := map[wemo.ID]*followed{}

	defer func() {
		stop()

		for _, one := range following {
			<-one.done
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		for _, listener := range s.listeners {
			_ = listener.Close()
		}
	}()

	err := wemo.Find(running, lent{s.ssdp}, func(found wemo.Found, lost bool) {
		// A plug told of again is one that moved or started again: what
		// it was asked to send is gone with what it was, and it is
		// followed anew.
		if was, known := following[found.ID]; known {
			was.stop()
			<-was.done
			delete(following, found.ID)
		}

		if lost {
			return
		}

		following[found.ID] = s.start(running, home, found)
	})

	if ctx.Err() != nil {
		// Stopped: what the browsing ended with is not the plugin's to say.
		return nil
	}

	return fmt.Errorf("finding Wemo plugs: %w", err)
}

// start follows a plug on a goroutine of its own, until it is stopped.
func (s *served) start(ctx context.Context, home *matter.Home, found wemo.Found) *followed {
	following, stop := context.WithCancel(ctx)
	one := &followed{stop: stop, done: make(chan struct{})}

	go func() {
		defer close(one.done)
		defer stop()

		s.follow(following, home, found)
	}()

	return one
}

// follow keeps a plug in the home for as long as ctx lasts: watched
// while it answers, unreachable while it does not, and reached for
// again.
func (s *served) follow(ctx context.Context, home *matter.Home, found wemo.Found) {
	ident := idOf(found.ID)

	for {
		err := s.watched(ctx, home, found)

		if _, put := home.Device(ident); put {
			_ = home.Report(ident, matter.ReachabilityChanged{Unreachable: true})
		}

		if ctx.Err() != nil {
			return
		}

		slog.WarnContext(ctx, "a Wemo plug is not there", "plug", string(found.ID), "error", err, "retry_in", s.retry)

		select {
		case <-ctx.Done():
			return
		case <-time.After(s.retry):
		}
	}
}

// watched is one watch of a plug, at the first place it answers at as
// the plug it says it is. It returns why the watch ended, or why there
// was none.
func (s *served) watched(ctx context.Context, home *matter.Home, found wemo.Found) error {
	failed := errNowhere

	for index, where := range found.Locations {
		if index == 0 {
			failed = nil
		}

		if !where.Local.IsValid() {
			failed = errors.Join(failed, fmt.Errorf("at %s: %w", where.URL, errNoWayBack))

			continue
		}

		plug, err := wemo.Open(ctx, s.client, where.URL)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("opening the plug at %s: %w", where.URL, err))

			continue
		}

		if plug.ID() != found.ID {
			failed = errors.Join(failed, fmt.Errorf("at %s: %w", where.URL, errAnother))

			continue
		}

		return s.heard(ctx, home, plug, where.Local)
	}

	return failed
}

// heard watches a plug that was opened, calling this host back at local,
// and keeps the home as the plug says it is. It returns why the watch
// ended.
func (s *served) heard(ctx context.Context, home *matter.Home, plug *wemo.Plug, local netip.Addr) error {
	events, err := s.listener(ctx, local)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.plugs[plug.ID()] = plug
	s.mu.Unlock()

	watching, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	// A plug that takes a subscription and cannot call back says nothing,
	// for ever: the wait for its first word is the plugin's to bound.
	silent := time.AfterFunc(s.retry, func() { stop(errSilent) })
	defer silent.Stop()

	callback := "http://" + netip.AddrPortFrom(local, events.Addr().Port()).String() + "/"

	err = plug.Watch(watching, events, callback, func(event wemo.Event) {
		silent.Stop()

		// A plug that cannot be placed ends the watch: a home that stays
		// as it was while the plug goes on is worse than one reached for
		// again.
		if on, _, said := event.State(); said {
			if unplaced := s.place(home, plug, on); unplaced != nil {
				stop(unplaced)
			}
		}
	})

	// What the plugin ended the watch for is why it ended.
	if cause := context.Cause(watching); cause != nil && ctx.Err() == nil {
		err = cause
	}

	return fmt.Errorf("watching the plug: %w", err)
}

// listener is where plugs send their events on one of this host's
// addresses: the one that is there, or a new one when there is none or
// it ended.
func (s *served) listener(ctx context.Context, local netip.Addr) (*wemo.Events, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if had, known := s.listeners[local]; known && had.Err() == nil {
		return had, nil
	}

	opened, err := wemo.Listen(ctx, netip.AddrPortFrom(local, s.port).String())
	if err != nil {
		return nil, fmt.Errorf("listening for the plugs' events: %w", err)
	}

	s.listeners[local] = opened

	return opened, nil
}

// place puts a plug into the home, or, when it is there, says it
// reachable and as it stands now. The home tells no one of what changes
// nothing.
func (s *served) place(home *matter.Home, plug *wemo.Plug, powered bool) error {
	ident := idOf(plug.ID())

	if _, had := home.Device(ident); !had {
		if err := home.Put(s.describe(home, plug, powered)); err != nil {
			return fmt.Errorf("putting the plug %s: %w", plug.ID(), err)
		}

		return nil
	}

	err := home.Report(ident, matter.ReachabilityChanged{Unreachable: false}, matter.OnOffChanged{On: powered})
	if err != nil {
		return fmt.Errorf("saying the plug %s is back: %w", plug.ID(), err)
	}

	return nil
}

// describe is one plug as a device of the vocabulary. Its command goes
// to the plug as it was last opened.
func (s *served) describe(home *matter.Home, plug *wemo.Plug, powered bool) matter.Device {
	said, which := plug.Description(), plug.ID()

	var described matter.Device

	described.ID, described.Name, described.Kind = idOf(which), said.Name, matter.Plug
	described.Vendor, described.Model = "Belkin", said.Model
	described.Serial, described.Firmware = said.Serial, said.Firmware

	described.OnOff = &matter.OnOff{
		On: powered,
		Set: func(ctx context.Context, wanted bool) error {
			s.mu.Lock()
			now := s.plugs[which]
			s.mu.Unlock()

			if err := now.SetState(ctx, wanted, 0); err != nil {
				return translate(err)
			}

			// The plug answered that it is switched: the home is told
			// now, whether or not the plug says it again by itself.
			_ = home.Report(described.ID, matter.OnOffChanged{On: wanted})

			return nil
		},
	}

	return described
}

// lent is the daemon's SSDP as go-wemo asks for the means of finding
// plugs.
type lent struct {
	ssdp plugin.SSDP
}

func (l lent) Browse(ctx context.Context, target string, handle func(wemo.Advertisement, bool)) error {
	return l.ssdp.Browse(ctx, target, func(change ssdp.Change, heard ssdp.Advertisement) {
		locations := make([]wemo.Location, 0, len(heard.Locations))
		for _, at := range heard.Locations {
			locations = append(locations, wemo.Location{URL: at.URL, Local: at.Local})
		}

		handle(wemo.Advertisement{USN: heard.USN, Locations: locations}, change == ssdp.Lost)
	})
}

// idOf is a plug's ID in the home: the plugin's name first, so that no
// other plugin's device has it.
func idOf(plug wemo.ID) matter.ID {
	return matter.ID("wemo/" + strings.TrimPrefix(string(plug), "uuid:"))
}

// translate puts go-wemo's errors in the vocabulary's.
func translate(err error) error {
	switch {
	case errors.Is(err, wemo.ErrArgument):
		return fmt.Errorf("%w: %w", matter.ErrInvalid, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", matter.ErrUnreachable, err)
	default:
		return fmt.Errorf("%w: %w", matter.ErrFailed, err)
	}
}
