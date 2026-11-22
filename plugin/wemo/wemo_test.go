package wemo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forkcloser/go-upnp/ssdp"
	"github.com/forkcloser/go-upnp/ssdp/ssdptest"

	"github.com/duncanite/go-matter"
	gowemo "github.com/duncanite/go-wemo/wemo"
	"github.com/duncanite/go-wemo/wemo/wemotest"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/plugin"
	"github.com/duncanite/matterialize/plugin/wemo"
)

// The fake plug as the home names it, and as it advertises itself.
const (
	ident matter.ID = "wemo/Socket-1_0-" + wemotest.Serial
	usn             = wemotest.UDN + "::" + gowemo.SearchTarget
)

// site is a network of a test: a LAN, and this host on it, at an address
// or with none on the plugs' network.
type site struct {
	lan      *ssdptest.LAN
	browsing *ssdp.Browser
}

// newSite puts the host on a LAN at an address, and on SSDP there.
func newSite(t *testing.T, at string) *site {
	t.Helper()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "lo0", 1, netip.MustParsePrefix(at))

	browsing, err := ssdp.Open(t.Context(), ssdp.Config{Network: host})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browsing.Close() })

	return &site{lan: lan, browsing: browsing}
}

// plugged is a plug on the site, advertising itself under a name where
// it is.
func (s *site) plugged(t *testing.T, name string) (*wemotest.Plug, *ssdptest.Device) {
	t.Helper()

	plug := wemotest.New()
	t.Cleanup(plug.Close)

	plug.On = true

	return plug, s.lan.Device(
		netip.MustParseAddr("127.0.0.1"),
		ssdptest.Advert{USN: name, Type: gowemo.SearchTarget, Location: plug.Location()},
	)
}

// settings is what the daemon hands the plugin: the settings written,
// and the SSDP it lends.
func settings(t *testing.T, browsing plugin.SSDP, written string) plugin.Settings {
	t.Helper()

	read, err := config.Parse([]byte("bridge: {name: Test, state: /state}\nplugins:\n  wemo: " + written + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	node := read.Plugins["wemo"]

	return plugin.Settings{Decode: config.Settings(&node), SSDP: browsing}
}

// running is the plugin under way against a home.
type running struct {
	home  matter.Home
	ended chan error
	stop  context.CancelFunc
}

// run makes the plugin on a site and runs it until the test ends, or
// until it is stopped.
func run(t *testing.T, on *site, written string) *running {
	t.Helper()

	made, err := wemo.New(settings(t, on.browsing, written))
	if err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(t.Context())
	started := &running{ended: make(chan error, 1), stop: stop}

	go func() { started.ended <- made.Run(ctx, &started.home) }()

	t.Cleanup(stop)

	return started
}

// stopped stops the plugin and waits for it to have let go of every
// plug: what a fake plug counted can be read from then on.
func (r *running) stopped(t *testing.T) {
	t.Helper()

	r.stop()

	select {
	case err := <-r.ended:
		if err != nil {
			t.Fatalf("the plugin, stopped, ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the plugin did not stop")
	}
}

// eventually waits for the home to hold what a test expects of the plug.
func eventually(t *testing.T, home *matter.Home, what string, holds func(now matter.Device, there bool) bool) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		now, there := home.Device(ident)
		if holds(now, there) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s: the home holds %+v (there: %t)", what, now, there)
		}
	}
}

// never waits long enough for a plug to have been put several times
// over, and fails when the home holds it.
func never(t *testing.T, home *matter.Home, what string) {
	t.Helper()

	time.Sleep(time.Second)

	if now, there := home.Device(ident); there {
		t.Fatalf("%s: the home holds %+v", what, now)
	}
}

// A plug that advertises itself on the network is a plug of the home,
// with what it says of itself and as it stands.
func TestAPlugOfTheNetwork(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	on.plugged(t, usn)

	started := run(t, on, "{}")

	eventually(t, &started.home, "a plug that is on", func(_ matter.Device, there bool) bool { return there })

	now, _ := started.home.Device(ident)

	if now.Name != wemotest.Name || now.Kind != matter.Plug || now.Vendor != "Belkin" || now.Model != "Socket" ||
		now.Serial != wemotest.Serial || now.Firmware != wemotest.Firmware {
		t.Errorf("the plug is described as %+v", now.Info)
	}

	if now.OnOff == nil || !now.OnOff.On || now.Unreachable || now.LevelControl != nil {
		t.Errorf("the plug stands as %+v", now)
	}
}

// What a plug says it is at, whoever switched it, is what the home
// holds.
func TestWhatAPlugSays(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	plug, _ := on.plugged(t, usn)

	started := run(t, on, "{}")

	eventually(t, &started.home, "a plug that is on", func(now matter.Device, there bool) bool {
		return there && now.OnOff.On
	})

	plug.Switch(t.Context(), false)
	eventually(t, &started.home, "the plug switched off at its button", func(now matter.Device, _ bool) bool {
		return !now.OnOff.On
	})

	plug.Switch(t.Context(), true)
	eventually(t, &started.home, "the plug switched on at its button", func(now matter.Device, _ bool) bool {
		return now.OnOff.On
	})
}

// A plug switched from the home is switched, and the home holds it so.
func TestAPlugIsSwitched(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	plug, _ := on.plugged(t, usn)

	started := run(t, on, "{}")

	eventually(t, &started.home, "a plug that is on", func(_ matter.Device, there bool) bool { return there })

	now, _ := started.home.Device(ident)

	if err := now.OnOff.Set(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	if now, _ = started.home.Device(ident); now.OnOff.On {
		t.Fatal("switched off from the home, the plug is on in it")
	}

	started.stopped(t)

	if plug.On || !slices.Contains(plug.Calls, "basicevent.SetBinaryState") {
		t.Fatalf("switched off from the home, the plug is on: %t, and was asked %v", plug.On, plug.Calls)
	}
}

// A plug that leaves the network is unreachable in the home, and stays
// in it; back, it is reachable, as it then stands.
func TestAPlugThatGoes(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	plug, device := on.plugged(t, usn)

	started := run(t, on, "{}")

	eventually(t, &started.home, "a plug that is on", func(_ matter.Device, there bool) bool { return there })

	device.Goodbye()
	eventually(t, &started.home, "the plug gone", func(now matter.Device, there bool) bool {
		return there && now.Unreachable
	})

	plug.Switch(t.Context(), false)
	device.Announce()
	eventually(t, &started.home, "the plug back, and off", func(now matter.Device, there bool) bool {
		return there && !now.Unreachable && !now.OnOff.On
	})
}

// A plug that started again has forgotten what it was asked to send: it
// is asked again.
func TestAPlugThatStartedAgain(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	plug, device := on.plugged(t, usn)

	device.Boot(1)

	started := run(t, on, "{}")

	eventually(t, &started.home, "a plug that is on", func(_ matter.Device, there bool) bool { return there })

	// The plug let go of and taken up again: unreachable, then not.
	told := make(chan bool, 16)

	go func() {
		_ = started.home.Watch(t.Context(), func(event matter.Event) {
			if said, about := event.Change.(matter.ReachabilityChanged); about {
				told <- said.Unreachable
			}
		})
	}()

	// A Watch tells what happens from when it began: it is given the
	// time to have begun.
	time.Sleep(500 * time.Millisecond)

	device.Boot(2)
	device.Announce()

	for _, unreachable := range []bool{true, false} {
		select {
		case said := <-told:
			if said != unreachable {
				t.Fatalf("the plug was said unreachable: %t, for %t", said, unreachable)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the plug was never said unreachable: %t", unreachable)
		}
	}

	started.stopped(t)

	if plug.Subscribed < 2 {
		t.Fatalf("a plug that started again was subscribed to %d times", plug.Subscribed)
	}
}

// A plug that takes a subscription and says nothing cannot call this
// host back: it is not put, and it is reached for again.
func TestAPlugThatSaysNothing(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	plug := wemotest.New()
	t.Cleanup(plug.Close)

	plug.Silent = true

	on.lan.Device(
		netip.MustParseAddr("127.0.0.1"),
		ssdptest.Advert{USN: usn, Type: gowemo.SearchTarget, Location: plug.Location()},
	)

	started := run(t, on, "{retry: 100ms}")

	never(t, &started.home, "a plug that says nothing")
	started.stopped(t)

	if plug.Subscribed < 2 {
		t.Fatalf("a plug that says nothing was subscribed to %d times", plug.Subscribed)
	}
}

// What answers where a plug says it is, and says it is another plug, is
// not taken for it: nothing is asked of it.
func TestAnotherPlugThere(t *testing.T) {
	t.Parallel()

	on := newSite(t, "127.0.0.1/8")
	plug, _ := on.plugged(t, "uuid:Socket-1_0-ANOTHER::"+gowemo.SearchTarget)

	started := run(t, on, "{retry: 100ms}")

	time.Sleep(time.Second)

	if devices, _ := started.home.Devices(t.Context()); len(devices) != 0 {
		t.Fatalf("the home holds %+v", devices)
	}

	started.stopped(t)

	if plug.Subscribed != 0 || len(plug.Calls) != 0 {
		t.Fatalf("another plug was subscribed to %d times and asked %v", plug.Subscribed, plug.Calls)
	}
}

// What answers where a plug says it is by pointing elsewhere is not a
// plug, and elsewhere hears nothing: an advertisement is anyone's to
// make, and sends no request where it did not say.
func TestAPlugThatPointsElsewhere(t *testing.T) {
	t.Parallel()

	var asked atomic.Int32

	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	t.Cleanup(elsewhere.Close)

	pointing := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/setup.xml", http.StatusFound))
	t.Cleanup(pointing.Close)

	on := newSite(t, "127.0.0.1/8")
	on.lan.Device(
		netip.MustParseAddr("127.0.0.1"),
		ssdptest.Advert{USN: usn, Type: gowemo.SearchTarget, Location: pointing.URL + "/setup.xml"},
	)

	started := run(t, on, "{retry: 100ms}")

	never(t, &started.home, "a plug that points elsewhere")
	started.stopped(t)

	if heard := asked.Load(); heard != 0 {
		t.Fatalf("where the plug pointed was asked %d times", heard)
	}
}

// A plug heard on a network this host has no address on has nothing to
// call back: it is not put, and nothing is asked of it.
func TestAPlugWithNoWayBack(t *testing.T) {
	t.Parallel()

	on := newSite(t, "192.0.2.10/24")
	plug, _ := on.plugged(t, usn)

	started := run(t, on, "{retry: 100ms}")

	never(t, &started.home, "a plug with no way back")
	started.stopped(t)

	if plug.Subscribed != 0 {
		t.Fatalf("a plug with no way back was subscribed to %d times", plug.Subscribed)
	}
}

// errGone is why a network can be browsed no more.
var errGone = errors.New("the last link broke")

// broken is an SSDP that can browse no more.
type broken struct{ why error }

func (b broken) Browse(context.Context, string, func(ssdp.Change, ssdp.Advertisement)) error {
	return b.why
}

// A network that can be browsed no more ends the plugin, with why.
func TestANetworkThatIsGone(t *testing.T) {
	t.Parallel()

	made, err := wemo.New(settings(t, broken{errGone}, "{}"))
	if err != nil {
		t.Fatal(err)
	}

	var home matter.Home

	if err = made.Run(t.Context(), &home); !errors.Is(err, errGone) {
		t.Fatalf("the plugin ended with %v", err)
	}
}

// Settings that say nothing are settings; ones that cannot be read are
// refused.
func TestSettings(t *testing.T) {
	t.Parallel()

	for _, written := range []string{"", "{}", "{retry: 3s}", "{port: 49200}"} {
		if _, err := wemo.New(settings(t, nil, written)); err != nil {
			t.Errorf("settings %q: %v", written, err)
		}
	}

	for _, written := range []string{"{retry: soon}", "{retry: -1s}", "{retry: 0s}"} {
		if _, err := wemo.New(settings(t, nil, written)); !errors.Is(err, plugin.ErrSettings) {
			t.Errorf("settings %q: %v", written, err)
		}
	}

	for _, written := range []string{"{host: 192.0.2.1}", "{port: 70000}", "{port: none}"} {
		if _, err := wemo.New(settings(t, nil, written)); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("settings %q: %v", written, err)
		}
	}
}
