package lent_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/forkcloser/go-mdns"
	"github.com/forkcloser/go-mdns/mdnstest"

	"github.com/duncanite/go-matter/bridge"

	"github.com/duncanite/matterialize/internal/lent"
)

// What a bridge is announced as, and where it answers.
const (
	commissionable = "_matterc._udp"
	operational    = "_matter._tcp"
)

// hardware is the hardware address of the interface the bridge of a
// test is announced on.
func hardware() net.HardwareAddr { return net.HardwareAddr{0x02, 0x00, 0x0a, 0x00, 0x02, 0x04} }

// hostname is what Matter has a host of that hardware address answer
// to.
func hostname(t *testing.T) string {
	t.Helper()

	name, err := bridge.HostName(hardware())
	if err != nil {
		t.Fatal(err)
	}

	return name
}

// lend puts the daemon of a test on a network, under that name.
func lend(t *testing.T, cfg lent.MDNSConfig) *lent.MDNS {
	t.Helper()

	cfg.Host = hostname(t)

	lending, err := lent.OpenMDNS(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	return lending
}

// The daemon's host: one interface on the wire of the things, one on the
// wire of the people, each with a routable address of both families and
// a link-local one.
func house(network *mdnstest.Network) *mdnstest.Host {
	return network.Host(
		mdnstest.Port{Interface: "eth0", Wire: "things", Addrs: prefixes("10.0.2.4/24", "fd00:2::4/64", "fe80::4/64")},
		mdnstest.Port{Interface: "eth1", Wire: "people", Addrs: prefixes("10.0.1.4/24", "fd00:1::4/64", "fe80::14/64")},
	)
}

func prefixes(written ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(written))
	for _, one := range written {
		out = append(out, netip.MustParsePrefix(one))
	}

	return out
}

// neighbour is a peer of its own on one wire: a controller, or a device.
func neighbour(t *testing.T, network *mdnstest.Network, wire, addr string) *mdns.Peer {
	t.Helper()

	peer, err := mdns.Open(t.Context(), mdns.Config{
		Hostname: "neighbour-" + wire + "-" + addr[len(addr)-4:len(addr)-3],
		Network:  network.Host(mdnstest.Port{Interface: "en0", Wire: wire, Addrs: prefixes(addr)}),
	})
	if err != nil {
		t.Fatal(err)
	}

	return peer
}

// commissioned is the bridge as go-matter announces it while it is open
// to a controller.
func commissioned(text string) bridge.Service {
	return bridge.Service{
		Name: "A1B2C3D4E5F60718", Type: commissionable, Subtypes: []string{"_L3840", "_S15", "_CM", "_T14"},
		Port: 5540,
		Text: []bridge.Entry{{Key: "D", Value: "3840"}, {Key: "CM", Value: "1"}, {Key: "DN", Value: text}},
	}
}

// heard is what a peer hears of a service, for a while: each instance as
// it last stood, and whether it was then lost.
type heard struct {
	last map[string]mdns.Instance
	lost map[string]bool
}

// listen has a peer browse, and gives what it has heard so far when
// asked. The browse ends with the test's bubble.
func listen(ctx context.Context, peer *mdns.Peer, query mdns.Query) func() heard {
	said := heard{last: map[string]mdns.Instance{}, lost: map[string]bool{}}

	go func() {
		_ = peer.Browse(ctx, query, func(change mdns.Change, instance mdns.Instance) {
			said.last[instance.Name], said.lost[instance.Name] = instance, change == mdns.Lost
		})
	}()

	return func() heard {
		synctest.Wait()

		return said
	}
}

// The bridge is announced on the interface it is given and on no other,
// under the name the bridge gives its host, at the addresses a
// controller there can reach, and withdrawn with a goodbye.
func TestTheBridgeIsAnnouncedWhereItIsToBe(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()

		var network mdnstest.Network

		lending := lend(t, lent.MDNSConfig{
			Bridge: "eth1", Plugins: []string{"eth0", "eth1"}, Network: house(&network),
		})
		defer func() { _ = lending.Close() }()

		people := neighbour(t, &network, "people", "10.0.1.9/24")
		defer func() { _ = people.Close() }()

		things := neighbour(t, &network, "things", "10.0.2.9/24")
		defer func() { _ = things.Close() }()

		among := listen(ctx, people, mdns.Query{Type: commissionable})
		narrowed := listen(ctx, people, mdns.Query{Type: commissionable, Subtype: "_L3840"})
		elsewhere := listen(ctx, things, mdns.Query{Type: commissionable})

		if err := lending.Announce(ctx, commissioned("Home")); err != nil {
			t.Fatal(err)
		}

		time.Sleep(10 * time.Second)

		found, there := among().last["A1B2C3D4E5F60718"]
		if !there || found.Host != hostname(t)+".local" || hostname(t) != "02000A000204" || found.Port != 5540 {
			t.Fatalf("where the bridge is announced, it is heard as %+v (there: %t)", found, there)
		}

		if said, _ := found.Text.Get("DN"); said != "Home" || len(found.Text) != 3 {
			t.Errorf("its text is heard as %v", found.Text)
		}

		slices.SortFunc(found.Addrs, netip.Addr.Compare)

		// The routable address of each family, and not the link-local one.
		if want := []netip.Addr{netip.MustParseAddr("10.0.1.4"), netip.MustParseAddr("fd00:1::4")}; !slices.Equal(
			found.Addrs,
			want,
		) {
			t.Errorf("its host is heard at %v, for %v", found.Addrs, want)
		}

		// Every record of it is remembered two minutes, the pointer to it
		// among them, which a service has remembered longer by default.
		var remembered []time.Duration

		go func() {
			_ = people.Ask(ctx, mdns.Question{Name: mdns.Name{"_matterc", "_udp", "local"}, Type: mdns.TypePTR},
				func(_ mdns.Change, answer mdns.Answer) { remembered = append(remembered, answer.Record.TTL) })
		}()

		synctest.Wait()

		if len(remembered) != 1 || remembered[0] != 2*time.Minute {
			t.Errorf("the pointer to it is remembered for %v", remembered)
		}

		if _, there = narrowed().last["A1B2C3D4E5F60718"]; !there {
			t.Error("it is not found under a subtype it is announced under")
		}

		if anything := elsewhere().last; len(anything) != 0 {
			t.Fatalf("where the bridge is not announced, it is heard: %+v", anything)
		}

		// What it says changes in place, and is told.
		if err := lending.Announce(ctx, commissioned("Hall")); err != nil {
			t.Fatal(err)
		}

		time.Sleep(10 * time.Second)

		if said, _ := among().last["A1B2C3D4E5F60718"].Text.Get("DN"); said != "Hall" {
			t.Errorf("changed, its text is heard as %q", said)
		}

		if err := lending.Withdraw(ctx, "A1B2C3D4E5F60718", commissionable); err != nil {
			t.Fatal(err)
		}

		time.Sleep(time.Second)

		if !among().lost["A1B2C3D4E5F60718"] {
			t.Error("withdrawn, the bridge said no goodbye")
		}

		// Withdrawing what is not announced is nothing.
		if err := lending.Withdraw(ctx, "A1B2C3D4E5F60718", commissionable); err != nil {
			t.Error(err)
		}
	})
}

// An interface whose only IPv6 address is the link-local one is
// announced at it: Matter is spoken over IPv6 first.
func TestALinkLocalAddressAlone(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()

		var network mdnstest.Network

		lending := lend(t, lent.MDNSConfig{Bridge: "eth0", Network: network.Host(
			mdnstest.Port{
				Interface: "eth0",
				Wire:      "things",
				Addrs:     prefixes("10.0.2.4/24", "fe80::4/64", "169.254.7.7/16"),
			},
		)})
		defer func() { _ = lending.Close() }()

		things := neighbour(t, &network, "things", "10.0.2.9/24")
		defer func() { _ = things.Close() }()

		among := listen(ctx, things, mdns.Query{Type: commissionable})

		if err := lending.Announce(ctx, commissioned("Home")); err != nil {
			t.Fatal(err)
		}

		time.Sleep(10 * time.Second)

		found := among().last["A1B2C3D4E5F60718"]

		addrs := make([]string, 0, len(found.Addrs))
		for _, at := range found.Addrs {
			addrs = append(addrs, at.WithZone("").String())
		}

		slices.Sort(addrs)

		if want := []string{"10.0.2.4", "fe80::4"}; !slices.Equal(addrs, want) {
			t.Fatalf("its host is heard at %v, for %v", addrs, want)
		}
	})
}

// With no interface named for it, a bridge is announced on every
// interface a controller could reach the host by, and not on one a
// container runtime made.
func TestABridgeOnEveryInterface(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()

		var network mdnstest.Network

		lending := lend(t, lent.MDNSConfig{Network: network.Host(
			mdnstest.Port{Interface: "eth0", Wire: "things", Addrs: prefixes("10.0.2.4/24")},
			mdnstest.Port{Interface: "eth1", Wire: "people", Addrs: prefixes("10.0.1.4/24")},
			mdnstest.Port{Interface: "docker0", Wire: "containers", Addrs: prefixes("172.17.0.1/16")},
		)})
		defer func() { _ = lending.Close() }()

		hears := map[string]func() heard{}

		var neighbours []*mdns.Peer

		defer func() {
			for _, peer := range neighbours {
				_ = peer.Close()
			}
		}()

		for wire, addr := range map[string]string{
			"things": "10.0.2.9/24", "people": "10.0.1.9/24", "containers": "172.17.0.9/16",
		} {
			peer := neighbour(t, &network, wire, addr)
			neighbours = append(neighbours, peer)

			hears[wire] = listen(ctx, peer, mdns.Query{Type: commissionable})
		}

		if err := lending.Announce(ctx, commissioned("Home")); err != nil {
			t.Fatal(err)
		}

		time.Sleep(10 * time.Second)

		for wire, want := range map[string]bool{"things": true, "people": true, "containers": false} {
			if _, there := hears[wire]().last["A1B2C3D4E5F60718"]; there != want {
				t.Errorf("on the wire of the %s the bridge is heard: %t", wire, there)
			}
		}
	})
}

// A plugin hears what is announced where the configuration has plugins
// listen, and nothing of what is announced elsewhere, whatever it asks;
// and it does from the moment the daemon is on the network, whether or
// not its bridge has announced anything.
func TestPluginsHearWhereTheyAreLetTo(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()

		var network mdnstest.Network

		lending := lend(t, lent.MDNSConfig{Bridge: "eth1", Plugins: []string{"eth0"}, Network: house(&network)})
		defer func() { _ = lending.Close() }()

		var neighbours []*mdns.Peer

		defer func() {
			for _, peer := range neighbours {
				_ = peer.Close()
			}
		}()

		for wire, addr := range map[string]string{"things": "10.0.2.9/24", "people": "10.0.1.9/24"} {
			device := neighbour(t, &network, wire, addr)
			neighbours = append(neighbours, device)

			if _, err := device.Announce(
				ctx,
				mdns.Service{Name: "of the " + wire, Type: "_thing._tcp", Port: 80},
			); err != nil {
				t.Fatal(err)
			}
		}

		found := map[string]bool{}
		everywhere := map[string]bool{}

		go func() {
			_ = lending.Browse(ctx, mdns.Query{Type: "_thing._tcp"}, func(_ mdns.Change, instance mdns.Instance) {
				found[instance.Name] = true
			})
		}()

		// A plugin that names an interface of its own is not listened to.
		go func() {
			_ = lending.Browse(ctx, mdns.Query{Type: "_thing._tcp", Interfaces: mdns.On("eth0", "eth1")},
				func(_ mdns.Change, instance mdns.Instance) { everywhere[instance.Name] = true })
		}()

		time.Sleep(10 * time.Second)
		synctest.Wait()

		for what, heard := range map[string]map[string]bool{"a plugin": found, "a plugin that names interfaces": everywhere} {
			if !heard["of the things"] || heard["of the people"] || len(heard) != 1 {
				t.Errorf("%s heard %v", what, heard)
			}
		}
	})
}

// A controller is found again where the bridge is announced, and not
// where it is not.
func TestAControllerIsFoundAgain(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(t.Context())
		defer stop()

		var network mdnstest.Network

		lending := lend(t, lent.MDNSConfig{Bridge: "eth1", Plugins: []string{"eth0"}, Network: house(&network)})
		defer func() { _ = lending.Close() }()

		var neighbours []*mdns.Peer

		defer func() {
			for _, peer := range neighbours {
				_ = peer.Close()
			}
		}()

		for wire, addr := range map[string]string{"things": "10.0.2.9/24", "people": "10.0.1.9/24"} {
			controller := neighbour(t, &network, wire, addr)
			neighbours = append(neighbours, controller)

			_, err := controller.Announce(ctx, mdns.Service{Name: "FAB-" + wire, Type: operational, Port: 5541})
			if err != nil {
				t.Fatal(err)
			}
		}

		if err := lending.Announce(ctx, commissioned("Home")); err != nil {
			t.Fatal(err)
		}

		at, err := lending.Resolve(ctx, "FAB-people", operational)
		if err != nil || !slices.Contains(at, netip.MustParseAddrPort("10.0.1.9:5541")) {
			t.Fatalf("a controller where the bridge is announced is found at %v, %v", at, err)
		}

		// Nothing on the network says an instance is not there: the search
		// lasts as long as it is given.
		brief, enough := context.WithTimeout(ctx, 30*time.Second)
		defer enough()

		if at, err = lending.Resolve(
			brief,
			"FAB-things",
			operational,
		); !errors.Is(err, context.DeadlineExceeded) ||
			at != nil {
			t.Fatalf("a controller where the bridge is not announced is found at %v, %v", at, err)
		}
	})
}

// What is closed stays closed, for the bridge and for the plugins.
func TestClosed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()

		var network mdnstest.Network

		lending := lend(t, lent.MDNSConfig{Bridge: "eth1", Network: house(&network)})

		if err := lending.Announce(ctx, commissioned("Home")); err != nil {
			t.Fatal(err)
		}

		if err := lending.Close(); err != nil {
			t.Fatal(err)
		}

		if err := lending.Close(); err != nil {
			t.Fatalf("closed twice: %v", err)
		}

		nothing := func(mdns.Change, mdns.Instance) {}

		if err := lending.Browse(ctx, mdns.Query{Type: "_thing._tcp"}, nothing); !errors.Is(err, mdns.ErrClosed) {
			t.Errorf("closed, a browse ended with %v", err)
		}

		if err := lending.Announce(ctx, commissioned("Hall")); !errors.Is(err, mdns.ErrClosed) {
			t.Errorf("closed, an announcement ended with %v", err)
		}
	})
}

// A host is named by the hardware address of the interface its bridge
// is announced on; with none named, by that of the first, by its index,
// of those a bridge is announced on that has one a name is made of. A
// host with none is refused: no name is made up.
func TestTheHostIsNamedByItsHardware(t *testing.T) {
	t.Parallel()

	const carries = net.FlagUp | net.FlagMulticast

	mac := func(last byte) net.HardwareAddr { return net.HardwareAddr{0x02, 0, 0, 0, 0, last} }
	host := []net.Interface{
		{Index: 9, Name: "eth9", Flags: carries, HardwareAddr: mac(9)},
		{Index: 1, Name: "lo", Flags: carries | net.FlagLoopback, HardwareAddr: mac(1)},
		{Index: 2, Name: "docker0", Flags: carries, HardwareAddr: mac(2)},
		{Index: 3, Name: "tun0", Flags: carries | net.FlagPointToPoint, HardwareAddr: mac(3)},
		{Index: 4, Name: "eth4", Flags: net.FlagMulticast, HardwareAddr: mac(4)},
		{Index: 5, Name: "eth5", Flags: carries},
		{Index: 6, Name: "eth6", Flags: carries, HardwareAddr: net.HardwareAddr{0, 0, 0, 0, 0, 0}},
		{Index: 7, Name: "eth7", Flags: carries, HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 0, 0, 7}},
		{Index: 8, Name: "eth8", Flags: carries, HardwareAddr: mac(8)},
	}

	for named, want := range map[string]string{
		"":     "0200000000000007", // the first by index that a bridge is announced on and that has one
		"eth8": "020000000008",
		"eth9": "020000000009",
		"eth4": "020000000004", // named, it is the operator's to say: it need not be up
	} {
		if name, err := lent.HostOf(host, named); err != nil || name != want {
			t.Errorf("with %q named, the host is %q, %v, for %q", named, name, err, want)
		}
	}

	for _, named := range []string{"eth5", "eth6", "nowhere0"} {
		if name, err := lent.HostOf(host, named); !errors.Is(err, lent.ErrNoHardware) || name != "" {
			t.Errorf("with %q named, the host is %q, %v", named, name, err)
		}
	}

	if name, err := lent.HostOf(host[1:7], ""); !errors.Is(err, lent.ErrNoHardware) || name != "" {
		t.Errorf("with no interface to be named by, the host is %q, %v", name, err)
	}
}
