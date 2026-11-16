// Package lutron is the plugin for Lutron Caséta: the zones of one
// Caséta bridge, each a light or a plug, written into the home and kept
// as the bridge says they are.
//
// Its settings:
//
//	lutron:
//	  # The Caséta bridge, by address or name; with a port, or LEAP's.
//	  host: 192.0.2.20
//	  # The credentials pairing with it gave, by the name of their
//	  # directory among the secrets. Left out, "lutron".
//	  credentials: lutron
//	  # How long before a bridge that is not there is reached for again.
//	  # Left out, ten seconds.
//	  retry: 10s
package lutron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/credentials"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-matter"

	"github.com/duncanite/matterialize/plugin"
)

// full is Lutron's level of a zone that is fully on, in percent.
const full = 100

// again is how long the plugin waits before it reaches for a Caséta
// bridge it lost, or could not reach, when its settings do not say.
const again = 10 * time.Second

// settings is the plugin's, as the configuration writes them.
type settings struct {
	Host        string `yaml:"host"`
	Credentials string `yaml:"credentials"`
	Retry       string `yaml:"retry"`
}

// served is the plugin, made.
type served struct {
	host     string
	identity *leap.Credentials
	retry    time.Duration

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
	read := settings{Credentials: "lutron"}

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

	dir, err := given.Secrets.Path(read.Credentials)
	if err != nil {
		return nil, err
	}

	identity, err := credentials.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: the credentials: %w", plugin.ErrSettings, err)
	}

	return &served{host: read.Host, identity: identity, retry: retry, put: map[string]bool{}}, nil
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

// connected is one connection to the Caséta bridge, from its opening to
// its end: the home brought to what the bridge says, and kept so. It
// returns why the connection ended.
func (s *served) connected(ctx context.Context, home *matter.Home) error {
	bridge, err := caseta.Open(ctx, s.host, s.identity)
	if err != nil {
		return fmt.Errorf("opening the Caséta bridge: %w", err)
	}

	s.bridge.Store(bridge)

	defer func() { _ = bridge.Close() }()

	if err = s.feed(ctx, bridge, home); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return nil
	case <-bridge.Conn().Done():
		return fmt.Errorf("the connection ended: %w", bridge.Conn().Err())
	}
}

// feed brings the home to the Caséta bridge's zones as they are now:
// one it had not put is put, one it had is said reachable again and as
// it stands, and one the bridge no longer has is removed. From then on
// what the bridge says of them is reported.
func (s *served) feed(ctx context.Context, bridge *caseta.Bridge, home *matter.Home) error {
	devices, err := bridge.Devices(ctx)
	if err != nil {
		return fmt.Errorf("listing the Lutron devices: %w", err)
	}

	// found is the zones served on this connection, and whether each has
	// a level.
	found := map[string]bool{}

	for index := range devices {
		device := &devices[index]

		dimmable, known := kindOf(device.DeviceType)
		if !known && len(device.LocalZones) > 0 {
			slog.InfoContext(ctx, "a device is left out: not a kind this plugin serves",
				"device", device.Name, "kind", device.DeviceType)

			continue
		}

		for _, driven := range device.LocalZones {
			zone := driven.ID()

			status, err := bridge.Zone(ctx, zone)
			if err != nil || status.Level == nil {
				slog.WarnContext(ctx, "a zone is left out", "zone", zone, "device", device.Name, "error", err)

				continue
			}

			if err = s.place(home, device, zone, dimmable, *status.Level); err != nil {
				return err
			}

			found[zone] = dimmable
		}
	}

	s.forget(home, found)
	bridge.WatchZones(func(changed caseta.ZoneStatus) { said(ctx, home, found, changed) })

	return nil
}

// forget takes out of the home the zones the plugin had put that the
// bridge no longer has.
func (s *served) forget(home *matter.Home, found map[string]bool) {
	for zone := range s.put {
		if _, still := found[zone]; !still {
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
// before, says it reachable again and as it stands now.
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

// said reports what the Caséta bridge says of a zone, whoever changed
// it: off is on/off's alone, and a zone that is off keeps its level.
// served is the zones of this connection, and whether each has a level.
func said(ctx context.Context, home *matter.Home, served map[string]bool, changed caseta.ZoneStatus) {
	zone := changed.Zone.ID()

	dimmable, known := served[zone]
	if !known || changed.Level == nil {
		return
	}

	var told []matter.Change

	switch {
	case *changed.Level == 0:
		told = []matter.Change{matter.OnOffChanged{On: false}}
	case dimmable:
		told = []matter.Change{matter.OnOffChanged{On: true}, matter.LevelChanged{Level: levelOf(*changed.Level)}}
	default:
		told = []matter.Change{matter.OnOffChanged{On: true}}
	}

	if err := home.Report(idOf(zone), told...); err != nil {
		slog.WarnContext(ctx, "a change of a zone could not be reported", "zone", zone, "error", err)
	}
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
	case errors.Is(err, caseta.ErrLevel):
		return fmt.Errorf("%w: %w", matter.ErrInvalid, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", matter.ErrUnreachable, err)
	default:
		return fmt.Errorf("%w: %w", matter.ErrFailed, err)
	}
}
