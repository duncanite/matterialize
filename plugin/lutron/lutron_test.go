package lutron_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-lutron/credentials"
	"github.com/duncanite/go-lutron/leap/leaptest"
	"github.com/duncanite/go-matter"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/plugin"
	"github.com/duncanite/matterialize/plugin/lutron"
)

// status is what a Caséta bridge says of a zone's level.
func status(zone string, level int) map[string]any {
	return map[string]any{"ZoneStatus": map[string]any{"Level": level, "Zone": map[string]any{"href": "/zone/" + zone}}}
}

// device is a device as a Caséta bridge lists it, driving one zone.
func device(id, room, name, kind, zone string) map[string]any {
	return map[string]any{
		"href": "/device/" + id, "Name": name, "FullyQualifiedName": []string{room, name}, "DeviceType": kind,
		"ModelNumber": "M-1", "SerialNumber": 42, "LocalZones": []any{map[string]any{"href": "/zone/" + zone}},
	}
}

// caseta is a Caséta bridge of a test with a dimmer that is lit, one
// that is off, a switch, an outdoor plug, a zone the bridge has no status
// of, one whose status has no level, and a shade and a fan, which have a
// level and are neither a dimmer nor a switch.
func caseta(t *testing.T) *leaptest.Bridge {
	t.Helper()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	bridge.Set("/device", "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{
		device("2", "Hall", "Lamp", "WallDimmer", "7"),
		device("3", "Porch", "Light", "WallSwitch", "8"),
		device("4", "Patio", "Outlet", "OutdoorPlugInSwitch", "9"),
		device("5", "Attic", "Mute", "WallSwitch", "10"),
		device("6", "Attic", "Levelless", "WallSwitch", "11"),
		device("7", "Den", "Sconce", "WallDimmer", "12"),
		device("8", "Den", "Shade", "SerenaHoneycombShade", "13"),
		device("9", "Den", "Fan", "CasetaFanSpeedController", "14"),
	}})
	bridge.Set("/zone/7/status", "OneZoneStatus", status("7", 40))
	bridge.Set("/zone/8/status", "OneZoneStatus", status("8", 0))
	bridge.Set("/zone/9/status", "OneZoneStatus", status("9", 100))
	bridge.Set("/zone/11/status", "OneZoneStatus", map[string]any{
		"ZoneStatus": map[string]any{"Zone": map[string]any{"href": "/zone/11"}},
	})
	bridge.Set("/zone/12/status", "OneZoneStatus", status("12", 0))
	bridge.Set("/zone/13/status", "OneZoneStatus", status("13", 55))
	bridge.Set("/zone/14/status", "OneZoneStatus", status("14", 50))

	return bridge
}

// settings is what the daemon hands a plugin: the settings written, and
// a directory of secrets holding the bridge's credentials as "lutron".
func settings(t *testing.T, bridge *leaptest.Bridge, written string) plugin.Settings {
	t.Helper()

	secrets := t.TempDir()

	reserved, err := credentials.Reserve(filepath.Join(secrets, "lutron"))
	if err != nil {
		t.Fatal(err)
	}

	if err = reserved.Save(bridge.Credentials()); err != nil {
		t.Fatal(err)
	}

	read, err := config.Parse([]byte("bridge: {name: Test, state: /state}\nplugins:\n  lutron: " + written + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	node := read.Plugins["lutron"]

	return plugin.Settings{Decode: config.Settings(&node), Secrets: plugin.Secrets{Dir: secrets}}
}

// running is the plugin under way against a home.
type running struct {
	home  matter.Home
	ended chan error
	stop  context.CancelFunc
}

// run makes the plugin for a bridge and runs it until the test ends,
// returning once the bridge's zones are in the home. retry is how long
// it waits before it reaches again for a bridge that went.
func run(t *testing.T, bridge *leaptest.Bridge, retry string) *running {
	t.Helper()

	made, err := lutron.New(settings(t, bridge, "{host: "+bridge.Addr()+", retry: "+retry+"}"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(t.Context())
	started := &running{ended: make(chan error, 1), stop: stop}

	go func() { started.ended <- made.Run(ctx, &started.home) }()

	t.Cleanup(stop)

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if devices, _ := started.home.Devices(ctx); len(devices) == 4 {
			return started
		}

		select {
		case err = <-started.ended:
			t.Fatalf("the plugin ended with %v", err)
		default:
		}

		if time.Now().After(deadline) {
			t.Fatal("the zones never came into the home")
		}
	}
}

// eventually waits for the home to hold what a test expects of a zone.
func eventually(t *testing.T, home *matter.Home, id matter.ID, what string, holds func(matter.Device) bool) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if now, _ := home.Device(id); holds(now) {
			return
		}

		if time.Now().After(deadline) {
			now, _ := home.Device(id)
			t.Fatalf("%s: the home holds %+v", what, now)
		}
	}
}

// The zones of a Caséta bridge are devices of the home: a dimmer a light
// with a level, a switch a light without, an outdoor switch a plug, each
// with what the bridge says of it. A zone the bridge says no level of is
// left out, and so is what is neither a dimmer nor a switch, whatever
// level it has: a shade is not a lamp.
func TestTheZonesOfABridge(t *testing.T) {
	t.Parallel()

	started := run(t, caseta(t), "1h")

	lamp, _ := started.home.Device("lutron/zone/7")
	light, _ := started.home.Device("lutron/zone/8")
	outlet, _ := started.home.Device("lutron/zone/9")

	if lamp.Info != (matter.Info{
		ID: "lutron/zone/7", Name: "Lamp", Kind: matter.Light, Room: "Hall",
		Vendor: "Lutron", Model: "M-1", Serial: "42",
	}) || !lamp.OnOff.On || lamp.LevelControl == nil || lamp.LevelControl.Level != 0.4 {
		t.Errorf("the dimmer: %+v, %+v, %+v", lamp.Info, lamp.OnOff, lamp.LevelControl)
	}

	if light.Kind != matter.Light || light.Room != "Porch" || light.OnOff.On || light.LevelControl != nil {
		t.Errorf("the switch: %+v, %+v", light.Info, light.OnOff)
	}

	if outlet.Kind != matter.Plug || !outlet.OnOff.On || outlet.LevelControl != nil {
		t.Errorf("the outdoor plug: %+v, %+v", outlet.Info, outlet.OnOff)
	}

	// Off when the plugin started, the bridge not saying where it stood:
	// it comes back to full.
	if sconce, _ := started.home.Device("lutron/zone/12"); sconce.OnOff.On || sconce.LevelControl == nil ||
		sconce.LevelControl.Level != 1 {
		t.Errorf("the dimmer that was off: %+v, %+v", sconce.OnOff, sconce.LevelControl)
	}

	for zone, why := range map[matter.ID]string{
		"lutron/zone/10": "of no status", "lutron/zone/11": "of no level",
		"lutron/zone/13": "a shade's", "lutron/zone/14": "a fan's",
	} {
		if _, held := started.home.Device(zone); held {
			t.Errorf("%s, %s, is in the home", zone, why)
		}
	}
}

// What a controller asks of a device is asked of its zone: off is level
// zero, on is the level it last had or full, a level is a percent over a
// time, and a dimmer is raised, lowered and stopped.
func TestWhatIsAskedOfAZone(t *testing.T) {
	t.Parallel()

	bridge := caseta(t)
	started := run(t, bridge, "1h")
	ctx := t.Context()

	lamp, _ := started.home.Device("lutron/zone/7")
	light, _ := started.home.Device("lutron/zone/8")

	for _, step := range []struct {
		what string
		ask  func() error
		want string
	}{
		{"the dimmer off", func() error { return lamp.OnOff.Set(ctx, false) }, `"Value":0`},
		{"the dimmer on again", func() error { return lamp.OnOff.Set(ctx, true) }, `"Value":40`},
		{
			"the dimmer to a quarter over three seconds",
			func() error { return lamp.LevelControl.Set(ctx, 0.25, 3*time.Second) },
			`"Level":25,"FadeTime":"00:00:03"`,
		},
		{"the dimmer to nearly nothing", func() error { return lamp.LevelControl.Set(ctx, 0.001, 0) }, `"Value":1}`},
		{"the dimmer raised", func() error { return lamp.LevelControl.Move(ctx, matter.Up) }, `"Raise"`},
		{"the dimmer lowered", func() error { return lamp.LevelControl.Move(ctx, matter.Down) }, `"Lower"`},
		{"the dimmer stopped", func() error { return lamp.LevelControl.Stop(ctx) }, `"Stop"`},
		{"the switch on", func() error { return light.OnOff.Set(ctx, true) }, `"Value":100`},
	} {
		before := len(bridge.Commands())

		if err := step.ask(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}

		sent := bridge.Commands()
		if len(sent) != before+1 || !strings.Contains(string(sent[before].Body), step.want) ||
			!strings.Contains(sent[before].URL, "/zone/") {
			t.Fatalf("%s: want %s, the bridge got %+v", step.what, step.want, sent[before:])
		}
	}
}

// What the Caséta bridge says of a zone, whoever changed it, is the
// home's: off keeps the level a dimmer had, on carries the new one, and
// a switch is on or off.
func TestWhatABridgeSaysOfAZone(t *testing.T) {
	t.Parallel()

	bridge := caseta(t)
	started := run(t, bridge, "1h")

	bridge.Unsolicited("/zone/7/status", "OneZoneStatus", status("7", 0))
	eventually(t, &started.home, "lutron/zone/7", "the dimmer switched off at the wall", func(now matter.Device) bool {
		return !now.OnOff.On && now.LevelControl.Level == 0.4
	})

	bridge.Unsolicited("/zone/7/status", "OneZoneStatus", status("7", 75))
	eventually(t, &started.home, "lutron/zone/7", "the dimmer brought to three quarters", func(now matter.Device) bool {
		return now.OnOff.On && now.LevelControl.Level == 0.75
	})

	bridge.Unsolicited("/zone/8/status", "OneZoneStatus", status("8", 100))
	eventually(t, &started.home, "lutron/zone/8", "the switch switched on", func(now matter.Device) bool {
		return now.OnOff.On
	})

	// A zone that is not served is said nothing of, and nothing breaks.
	bridge.Unsolicited("/zone/10/status", "OneZoneStatus", status("10", 100))
	bridge.Unsolicited("/zone/8/status", "OneZoneStatus", status("8", 0))
	eventually(t, &started.home, "lutron/zone/8", "the switch switched off", func(now matter.Device) bool {
		return !now.OnOff.On
	})
}

// The plugin runs for as long as it is let, and ends with nothing to
// say when it is stopped. A Caséta bridge that goes away does not end
// it: its devices are unreachable in the home, and asked nothing.
func TestABridgeThatGoesAway(t *testing.T) {
	t.Parallel()

	bridge := caseta(t)
	started := run(t, bridge, "1h")

	lamp, _ := started.home.Device("lutron/zone/7")

	bridge.Drop()

	for _, zone := range []matter.ID{"lutron/zone/7", "lutron/zone/8", "lutron/zone/9", "lutron/zone/12"} {
		eventually(t, &started.home, zone, "the bridge gone", func(now matter.Device) bool { return now.Unreachable })
	}

	if err := lamp.OnOff.Set(t.Context(), false); !errors.Is(err, matter.ErrUnreachable) {
		t.Errorf("asked while the bridge is away: %v", err)
	}

	select {
	case err := <-started.ended:
		t.Fatalf("the bridge gone, the plugin ended with %v", err)
	default:
	}

	started.stop()

	if err := <-started.ended; err != nil {
		t.Errorf("stopped, the plugin ended with %v", err)
	}
}

// A Caséta bridge that went is reached for again, and when it answers
// its devices are as it says they are now: reachable, in the state they
// have, the ones it no longer has gone and the ones it has gained there.
// A device read before it went is the one the home still holds.
func TestABridgeThatComesBack(t *testing.T) {
	t.Parallel()

	bridge := caseta(t)
	started := run(t, bridge, "50ms")
	ctx := t.Context()

	lamp, _ := started.home.Device("lutron/zone/7")

	// What the bridge is when it is reached again: the lamp brighter,
	// the outlet off, the porch light gone, a new dimmer.
	bridge.Set("/device", "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{
		device("2", "Hall", "Lamp", "WallDimmer", "7"),
		device("4", "Patio", "Outlet", "OutdoorPlugInSwitch", "9"),
		device("7", "Den", "Sconce", "WallDimmer", "12"),
		device("10", "Hall", "Spots", "WallDimmer", "15"),
	}})
	bridge.Set("/zone/7/status", "OneZoneStatus", status("7", 80))
	bridge.Set("/zone/9/status", "OneZoneStatus", status("9", 0))
	bridge.Set("/zone/15/status", "OneZoneStatus", status("15", 30))
	bridge.Drop()

	eventually(t, &started.home, "lutron/zone/15", "the bridge back with a new dimmer", func(now matter.Device) bool {
		return now.Name == "Spots" && now.LevelControl != nil && now.LevelControl.Level == 0.3
	})
	eventually(t, &started.home, "lutron/zone/7", "the bridge back", func(now matter.Device) bool {
		return !now.Unreachable && now.OnOff.On && now.LevelControl.Level == 0.8
	})
	eventually(t, &started.home, "lutron/zone/12", "the bridge back, a dimmer still off", func(now matter.Device) bool {
		return !now.Unreachable && !now.OnOff.On && now.LevelControl.Level == 1
	})

	eventually(
		t,
		&started.home,
		"lutron/zone/9",
		"the bridge back, a plug switched off meanwhile",
		func(now matter.Device) bool {
			return !now.Unreachable && !now.OnOff.On
		},
	)

	if _, held := started.home.Device("lutron/zone/8"); held {
		t.Error("a zone the bridge no longer has is in the home")
	}

	before := len(bridge.Commands())

	if err := lamp.OnOff.Set(ctx, false); err != nil {
		t.Fatalf("asked of a device read before the bridge went: %v", err)
	}

	if sent := bridge.Commands(); len(sent) != before+1 || !strings.Contains(string(sent[before].Body), `"Value":0`) {
		t.Errorf("the bridge got %+v", sent[before:])
	}
}

// What the plugin cannot be made from is refused before anything is
// reached for.
func TestWhatThePluginRefuses(t *testing.T) {
	t.Parallel()

	bridge := caseta(t)

	for what, row := range map[string]struct {
		written string
		want    error
		says    string
	}{
		"no host":                 {"{}", plugin.ErrSettings, "it has no host"},
		"a key that is none":      {"{host: h, port: 8081}", config.ErrInvalid, "port"},
		"credentials not there":   {"{host: h, credentials: elsewhere}", plugin.ErrSettings, "the credentials"},
		"credentials out of it":   {"{host: h, credentials: ../lutron}", plugin.ErrSettings, "the secret"},
		"a retry that is no time": {"{host: h, retry: soon}", plugin.ErrSettings, "retry is a length of time"},
		"a retry of no time":      {"{host: h, retry: 0s}", plugin.ErrSettings, "retry is a length of time"},
	} {
		if _, err := lutron.New(settings(t, bridge, row.written)); !errors.Is(err, row.want) ||
			!strings.Contains(err.Error(), row.says) {
			t.Errorf("%s: %v", what, err)
		}
	}

	// A host no bridge answers at is not refused: it is reached for
	// until the plugin is stopped, and then the plugin has nothing to say.
	made, err := lutron.New(settings(t, bridge, "{host: '127.0.0.1:1', retry: 10ms}"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()

	var home matter.Home

	if err = made.Run(ctx, &home); err != nil {
		t.Errorf("a host no bridge answers at: %v", err)
	}
}
