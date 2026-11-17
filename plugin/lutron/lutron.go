// Package lutron is the plugin for Lutron Caséta: the zones of one
// Caséta bridge, each a light or a plug, written into the home and kept
// as the bridge says they are.
//
// Its settings:
//
//	lutron:
//	  # The Caséta bridge, by address or name; with a port, or LEAP's.
//	  host: 192.0.2.20
//	  # Where the bridge takes a pairing, when that is not the same host
//	  # on the pairing port.
//	  pairing: 192.0.2.20:8083
//	  # How long before a bridge that is not there is reached for again.
//	  # Left out, ten seconds.
//	  retry: 10s
//
// The plugin pairs with the bridge by itself, the first time it runs: it
// says so in the log, and waits for the button on the bridge to be
// pressed. What the pairing gave is kept in the plugin's store as
// "credentials": the key, the certificate the bridge signed and the
// bridge's root, three PEM blocks in that order. With it taken out of the
// store, the plugin pairs again; with a pairing made elsewhere put there,
// it does not pair at all.
//
// A pairing port that wants another identity than Lutron's Caséta app's
// is given it the same way, kept as "app".
package lutron

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-matter"

	"github.com/duncanite/matterialize/plugin"
)

// full is Lutron's level of a zone that is fully on, in percent.
const full = 100

// again is how long the plugin waits before it reaches for a Caséta
// bridge it lost, or could not reach, when its settings do not say.
const again = 10 * time.Second

// What the plugin keeps in its store, and how it is called on the bridge
// it pairs with.
const (
	credentials = "credentials"
	app         = "app"
	client      = "matterialize"
)

// errStored is what the store holds that is not the three PEM blocks of
// an identity.
var errStored = errors.New("not three PEM blocks: a key, a certificate and a root")

// settings is the plugin's, as the configuration writes them.
type settings struct {
	Host    string `yaml:"host"`
	Pairing string `yaml:"pairing"`
	Retry   string `yaml:"retry"`
}

// served is the plugin, made.
type served struct {
	host    string
	pairing string
	store   plugin.Store
	retry   time.Duration

	// bridge is the connection to the Caséta bridge a device's commands
	// are sent on: the last one made, set before any device is put. One
	// that has ended answers with its error; the home asks nothing of a
	// device it holds unreachable, which is all of them a moment later.
	bridge atomic.Pointer[caseta.Bridge]
	// put is the zones the plugin has put into the home, and whether
	// each has a level. It is Run's alone.
	put map[string]bool
}

// New makes the plugin from its settings: it is the Maker to enable
// under the name "lutron".
func New(given plugin.Settings) (plugin.Plugin, error) {
	var read settings

	if err := given.Decode(&read); err != nil {
		return nil, fmt.Errorf("its settings: %w", err)
	}

	if read.Host == "" {
		return nil, fmt.Errorf("%w: it has no host", plugin.ErrSettings)
	}

	retry := again

	if read.Retry != "" {
		var err error

		if retry, err = time.ParseDuration(read.Retry); err != nil || retry <= 0 {
			return nil, fmt.Errorf("%w: retry is a length of time, as 10s is, not %q", plugin.ErrSettings, read.Retry)
		}
	}

	pairing := read.Pairing

	if pairing == "" {
		name := read.Host

		// The pairing port is the same host's, whatever port LEAP is asked on.
		if alone, _, err := net.SplitHostPort(name); err == nil {
			name = alone
		}

		pairing = net.JoinHostPort(strings.Trim(name, "[]"), leap.PairingPort)
	}

	return &served{
		host: read.Host, pairing: pairing, store: given.Store, retry: retry, put: map[string]bool{},
	}, nil
}

// Run is Plugin's. It ends with ctx and with nothing else: a Caséta
// bridge that cannot be reached, or that goes away, is reached for
// again, its devices unreachable in the home until it answers.
func (s *served) Run(ctx context.Context, home *matter.Home) error {
	for {
		err := s.connected(ctx, home)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // stopped: whatever the connection ended with is not the plugin's to say
		}

		slog.WarnContext(
			ctx,
			"the Caséta bridge is not there; its devices are unreachable",
			"error",
			err,
			"retry_in",
			s.retry,
		)

		for zone := range s.put {
			_ = home.Report(idOf(zone), matter.ReachabilityChanged{Unreachable: true})
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.retry):
		}
	}
}

// identity is what the Caséta bridge lets the plugin in under: what the
// store holds, and, when it holds nothing, what a pairing gives.
func (s *served) identity(ctx context.Context) (*leap.Credentials, error) {
	held, err := s.kept(ctx, credentials)
	if held != nil || err != nil {
		return held, err
	}

	under, err := s.kept(ctx, app)
	if err != nil {
		return nil, err
	}

	if under == nil {
		under = leap.CasetaApp()
	}

	config, err := leap.TLSConfig(under)
	if err != nil {
		return nil, fmt.Errorf("the identity to pair under: %w", err)
	}

	conn, err := leap.DialTLS(ctx, s.pairing, config)
	if err != nil {
		return nil, fmt.Errorf("pairing with the Caséta bridge: %w", err)
	}

	given, err := leap.Pair(ctx, conn, client, func() {
		slog.WarnContext(
			ctx, "not paired with the Caséta bridge yet: press the button on the back of it", "bridge", s.pairing,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("pairing with the Caséta bridge: %w", err)
	}

	// A pairing that is not kept is asked for again at every start, and
	// leaves one more client on the bridge each time: it is no pairing.
	sealed := slices.Concat(given.Key, given.Certificate, given.Root)
	if err = s.store.Put(ctx, credentials, sealed); err != nil {
		return nil, fmt.Errorf("keeping the pairing: %w", err)
	}

	slog.InfoContext(ctx, "paired with the Caséta bridge", "bridge", s.pairing)

	return given, nil
}

// kept is an identity the store holds under a name, and nil when it
// holds nothing there.
func (s *served) kept(ctx context.Context, name string) (*leap.Credentials, error) {
	held, err := s.store.Get(ctx, name)
	if errors.Is(err, plugin.ErrNotStored) {
		return nil, nil //nolint:nilnil // nothing kept is no error, and no identity
	}

	if err != nil {
		return nil, fmt.Errorf("reading %s from the store: %w", name, err)
	}

	var blocks [3][]byte

	for index := range blocks {
		var block *pem.Block

		if block, held = pem.Decode(held); block == nil {
			return nil, fmt.Errorf("what the store holds as %s: %w", name, errStored)
		}

		blocks[index] = pem.EncodeToMemory(block)
	}

	return &leap.Credentials{Key: blocks[0], Certificate: blocks[1], Root: blocks[2]}, nil
}

// connected is one connection to the Caséta bridge, from its opening to
// its end: the home brought to what the bridge says, and kept so. It
// returns why the connection ended.
func (s *served) connected(ctx context.Context, home *matter.Home) error {
	identity, err := s.identity(ctx)
	if err != nil {
		return err
	}

	bridge, err := caseta.Open(ctx, s.host, identity)
	if err != nil {
		return fmt.Errorf("opening the Caséta bridge: %w", err)
	}

	s.bridge.Store(bridge)

	defer func() { _ = bridge.Close() }()

	return s.watch(ctx, bridge, home)
}

// watch brings the home to the Caséta bridge's zones as they stand, and
// keeps it so for as long as the connection lasts: a zone the plugin had
// not put is put, one it had is said reachable again and as it stands,
// one the bridge no longer has is removed, and from then on what the
// bridge says of each is reported. It returns why the watch ended.
func (s *served) watch(ctx context.Context, bridge *caseta.Bridge, home *matter.Home) error {
	// drives is each zone of a kind the plugin serves, with the device
	// that drives it.
	drives := map[string]caseta.Device{}

	// A zone that cannot be placed ends the watch: a home that stays as it
	// was while the bridge goes on is worse than one reached for again.
	watching, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	err := bridge.Watch(watching, func(event caseta.Event) {
		switch said := event.(type) {
		case caseta.Device:
			list(ctx, drives, said)
		case caseta.Settled:
			s.forget(home, drives)
		case caseta.ZoneStatus:
			if failed := s.told(ctx, home, drives, said); failed != nil {
				stop(failed)
			}
		default:
		}
	})

	return fmt.Errorf("watching the Caséta bridge: %w", err)
}

// list notes the zones a device drives, when it is of a kind the plugin
// serves.
func list(ctx context.Context, drives map[string]caseta.Device, device caseta.Device) {
	if _, known := kindOf(device.DeviceType); known {
		for _, driven := range device.LocalZones {
			drives[driven.ID()] = device
		}

		return
	}

	if len(device.LocalZones) > 0 {
		slog.InfoContext(ctx, "a device is left out: not a kind this plugin serves",
			"device", device.Name, "kind", device.DeviceType)
	}
}

// told is what the Caséta bridge says a zone is at, the first time and
// every time after, whoever changed it.
func (s *served) told(
	ctx context.Context,
	home *matter.Home,
	drives map[string]caseta.Device,
	status caseta.ZoneStatus,
) error {
	zone := status.Zone.ID()

	device, ours := drives[zone]
	if !ours {
		return nil
	}

	if status.Level == nil {
		slog.WarnContext(ctx, "the bridge gives a zone no level", "zone", zone, "device", device.Name)

		return nil
	}

	dimmable, _ := kindOf(device.DeviceType)

	return s.place(home, &device, zone, dimmable, *status.Level)
}

// forget takes out of the home the zones the plugin had put that the
// bridge no longer has.
func (s *served) forget(home *matter.Home, drives map[string]caseta.Device) {
	for zone := range s.put {
		if _, still := drives[zone]; !still {
			home.Remove(idOf(zone))
			delete(s.put, zone)
		}
	}
}

// kindOf says whether a device of the bridge's kind is one the plugin
// serves, and whether it has a level: dimmers and switches, in the wall
// or plugged in. A shade or a fan has a level too and is neither.
func kindOf(kind string) (dimmable, served bool) {
	return strings.Contains(kind, "Dimmer"), strings.Contains(kind, "Dimmer") || strings.Contains(kind, "Switch")
}

// place puts a zone into the home, or, when the plugin had put it
// before, says it reachable and as it stands now: off is on/off's alone,
// and a zone that is off keeps its level. The home tells no one of what
// changes nothing.
func (s *served) place(home *matter.Home, device *caseta.Device, zone string, dimmable bool, level int) error {
	if _, had := s.put[zone]; !had {
		if err := home.Put(s.describe(home, device, zone, dimmable, level)); err != nil {
			return fmt.Errorf("putting the zone %s: %w", zone, err)
		}

		s.put[zone] = dimmable

		return nil
	}

	told := []matter.Change{matter.ReachabilityChanged{Unreachable: false}, matter.OnOffChanged{On: level > 0}}
	if dimmable && level > 0 {
		told = append(told, matter.LevelChanged{Level: levelOf(level)})
	}

	if err := home.Report(idOf(zone), told...); err != nil {
		return fmt.Errorf("saying the zone %s is back: %w", zone, err)
	}

	return nil
}

// level sets a zone's level on the connection the plugin has.
func (s *served) level(ctx context.Context, zone string, percent int, over time.Duration) error {
	return translate(s.bridge.Load().SetLevel(ctx, zone, percent, over))
}

// describe is one zone as a device of the vocabulary. Its commands go to
// whichever connection the plugin has when they are asked.
func (s *served) describe(
	home *matter.Home,
	device *caseta.Device,
	zone string,
	dimmable bool,
	level int,
) matter.Device {
	ident := idOf(zone)

	var described matter.Device

	described.ID, described.Name, described.Kind = ident, device.Name, matter.Light
	described.Vendor, described.Model, described.Firmware = "Lutron", device.ModelNumber, device.Firmware

	if device.SerialNumber != 0 {
		described.Serial = strconv.FormatInt(device.SerialNumber, 10)
	}

	// The room is the first of the names the bridge qualifies it by.
	if len(device.FullyQualifiedName) > 1 {
		described.Room = device.FullyQualifiedName[0]
	}

	if strings.Contains(device.DeviceType, "PlugIn") || strings.Contains(device.DeviceType, "Outdoor") {
		described.Kind = matter.Plug
	}

	described.OnOff = &matter.OnOff{
		On: level > 0,
		Set: func(ctx context.Context, on bool) error {
			wanted := 0

			if on {
				wanted = full

				// Back to the level it last had, which the home kept.
				if last, held := home.Device(ident); held && last.LevelControl != nil {
					wanted = percentOf(last.LevelControl.Level)
				}
			}

			return s.level(ctx, zone, wanted, 0)
		},
	}

	if !dimmable {
		return described
	}

	// A zone that is off comes back to full until it has been seen lit:
	// the Caséta bridge does not say where it stood.
	stood := matter.Level(1)
	if level > 0 {
		stood = levelOf(level)
	}

	described.LevelControl = &matter.LevelControl{
		Level: stood,
		Set: func(ctx context.Context, level matter.Level, over time.Duration) error {
			return s.level(ctx, zone, percentOf(level), over)
		},
		Move: func(ctx context.Context, direction matter.Direction) error {
			if direction == matter.Down {
				return translate(s.bridge.Load().Lower(ctx, zone))
			}

			return translate(s.bridge.Load().Raise(ctx, zone))
		},
		Stop: func(ctx context.Context) error {
			return translate(s.bridge.Load().Stop(ctx, zone))
		},
	}

	return described
}

// idOf is a zone's ID in the home: the plugin's name first, so that no
// other plugin's device has it.
func idOf(zone string) matter.ID { return matter.ID("lutron/zone/" + zone) }

// levelOf is Lutron's percent as a level, never zero: zero is off.
func levelOf(percent int) matter.Level {
	return matter.Level(min(max(percent, 1), full)) / full
}

// percentOf is a level in Lutron's unit, never zero.
func percentOf(level matter.Level) int {
	return min(max(int(math.Round(float64(level)*full)), 1), full)
}

// translate puts go-lutron's errors in the vocabulary's.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, caseta.ErrArgument):
		return fmt.Errorf("%w: %w", matter.ErrInvalid, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", matter.ErrUnreachable, err)
	default:
		return fmt.Errorf("%w: %w", matter.ErrFailed, err)
	}
}
