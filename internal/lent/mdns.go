package lent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/forkcloser/go-mdns"
	"github.com/forkcloser/go-mdns/transport"

	"github.com/duncanite/go-matter/bridge"
)

// lifetime is how long a neighbour may remember a record of the bridge
// after it last heard it: what bridge.Network has every record live.
const lifetime = 120 * time.Second

// ErrNoHardware is a host with no hardware address to be named by.
var ErrNoHardware = errors.New("the host has no hardware address to be named by")

// MDNSConfig says what the daemon's part in Multicast DNS answers to,
// and where it is used, for what.
type MDNSConfig struct {
	// Host is the name the host answers to, which the bridge is
	// announced under: one HostOf gives.
	Host string
	// Bridge is the interface the bridge is announced on and looks for
	// its controllers on. Empty is every interface a controller could
	// reach the host by: neither a point-to-point one nor one a
	// container runtime made.
	Bridge string
	// Plugins is the interfaces plugins hear on. Nil is every one.
	Plugins []string
	// Network is where the links are. Nil is the host's own.
	Network transport.Network
}

// MDNS is the daemon's part in Multicast DNS: one mdns.Peer, which
// announces the bridge, finds the bridge its controllers again, and
// browses for the plugins, each on the interfaces the configuration
// gives that use. It is a bridge.Network and a plugin.MDNS.
type MDNS struct {
	cfg   MDNSConfig
	peer  *mdns.Peer
	links *links

	mu   sync.Mutex
	held map[[2]string]*announced
}

// announced is one service of the bridge, as it stands on the network.
type announced struct {
	service bridge.Service
	said    *mdns.Announcement
}

// HostOf is the name a host answers to as Matter has it: the one
// bridge.HostName makes of the hardware address of the interface the
// bridge is announced on. With no interface named for the bridge, it is
// that of the first, by its index, of the interfaces a bridge is then
// announced on that has a hardware address a name can be made of: one
// that is up and carries multicast, not the loopback, not a
// point-to-point one, not one a container runtime made. A host with
// none is ErrNoHardware: no name is made up for it.
func HostOf(interfaces []net.Interface, named string) (string, error) {
	interfaces = slices.Clone(interfaces)
	slices.SortFunc(interfaces, func(one, other net.Interface) int { return one.Index - other.Index })

	for _, candidate := range interfaces {
		if named != "" && candidate.Name != named {
			continue
		}

		const carries = net.FlagUp | net.FlagMulticast

		if named == "" && (candidate.Flags&carries != carries ||
			candidate.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 || made(candidate.Name)) {
			continue
		}

		if host, err := bridge.HostName(candidate.HardwareAddr); err == nil {
			return host, nil
		}
	}

	if named != "" {
		return "", fmt.Errorf("%w: the interface %q has none", ErrNoHardware, named)
	}

	return "", ErrNoHardware
}

// OpenMDNS puts the daemon on Multicast DNS, under the name cfg gives
// its host: from then on plugins browse, and the bridge is announced
// when it asks to be.
func OpenMDNS(ctx context.Context, cfg MDNSConfig) (*MDNS, error) {
	inner := cfg.Network
	if inner == nil {
		inner = transport.System()
	}

	opened := &MDNS{
		cfg:   cfg,
		links: &links{inner: inner, addrs: map[transport.Link][]netip.Prefix{}},
		held:  map[[2]string]*announced{},
	}

	opening := mdns.Config{
		Hostname:  cfg.Host,
		Rename:    mdns.Never,
		Addresses: opened.links.reaches,
		Network:   opened.links,
	}

	// On every interface, unless both uses name theirs: a use that names
	// none is on all of them.
	if cfg.Bridge != "" && cfg.Plugins != nil {
		opening.Interfaces = mdns.On(append(slices.Clone(cfg.Plugins), cfg.Bridge)...)
	}

	peer, err := mdns.Open(ctx, opening)
	if err != nil {
		return nil, fmt.Errorf("opening Multicast DNS: %w", err)
	}

	opened.peer = peer

	return opened, nil
}

// Announce is bridge.Network's.
func (m *MDNS) Announce(ctx context.Context, service bridge.Service) error {
	key := [2]string{service.Name, service.Type}
	text := make(mdns.TXT, 0, len(service.Text))

	for _, entry := range service.Text {
		text = append(text, mdns.Entry{Key: entry.Key, Value: []byte(entry.Value)})
	}

	m.mu.Lock()
	was := m.held[key]
	m.mu.Unlock()

	if was != nil {
		moved := was.service.Port != service.Port || !slices.Equal(was.service.Subtypes, service.Subtypes)

		switch {
		case !moved && slices.Equal(was.service.Text, service.Text):
			// As it was: a record that has not changed is not said again.
			return nil
		case !moved:
			if err := was.said.Update(ctx, text); err != nil {
				return fmt.Errorf("changing what %s.%s says: %w", service.Name, service.Type, err)
			}

			m.mu.Lock()
			was.service = service
			m.mu.Unlock()

			return nil
		default:
			_ = was.said.Close()

			m.mu.Lock()
			delete(m.held, key)
			m.mu.Unlock()
		}
	}

	said, err := m.peer.Announce(ctx, mdns.Service{
		Name: service.Name, Type: service.Type, Subtypes: service.Subtypes, Port: service.Port, Text: text,
		Interfaces: m.bridged(), Rename: mdns.Never, TTL: lifetime,
	})
	if err != nil {
		return fmt.Errorf("announcing %s.%s: %w", service.Name, service.Type, err)
	}

	m.mu.Lock()
	m.held[key] = &announced{service: service, said: said}
	m.mu.Unlock()

	return nil
}

// Withdraw is bridge.Network's.
func (m *MDNS) Withdraw(_ context.Context, name, kind string) error {
	key := [2]string{name, kind}

	m.mu.Lock()
	was := m.held[key]
	delete(m.held, key)
	m.mu.Unlock()

	if was == nil {
		return nil
	}

	if err := was.said.Close(); err != nil {
		return fmt.Errorf("withdrawing %s.%s: %w", name, kind, err)
	}

	return nil
}

// Resolve is bridge.Network's. Nothing on the network says an instance
// is not there: it is looked for, on the interfaces the bridge is
// announced on, until it is found or ctx ends.
func (m *MDNS) Resolve(ctx context.Context, name, kind string) ([]netip.AddrPort, error) {
	looking, stop := context.WithCancel(ctx)
	defer stop()

	var found []netip.AddrPort

	err := m.peer.Browse(looking, mdns.Query{Type: kind, Name: name, Interfaces: m.bridged()},
		func(change mdns.Change, instance mdns.Instance) {
			if change == mdns.Lost || len(instance.Addrs) == 0 || found != nil {
				return
			}

			for _, at := range instance.Addrs {
				found = append(found, netip.AddrPortFrom(at, instance.Port))
			}

			stop()
		})

	if found != nil {
		return found, nil
	}

	return nil, fmt.Errorf("looking for %s.%s: %w", name, kind, err)
}

// Browse is plugin.MDNS's.
func (m *MDNS) Browse(ctx context.Context, query mdns.Query, handle func(mdns.Change, mdns.Instance)) error {
	// A plugin chooses no interface: what a query names is listened on
	// only where the configuration lets plugins listen.
	if allowed, asked := m.cfg.Plugins, query.Interfaces; allowed != nil {
		query.Interfaces = func(candidate mdns.Interface) bool {
			return slices.Contains(allowed, candidate.Name) && (asked == nil || asked(candidate))
		}
	}

	//nolint:wrapcheck // the Peer's own word, handed on as it is
	return m.peer.Browse(ctx, query, handle)
}

// Close takes the daemon off Multicast DNS, with a goodbye for what it
// announced: every Browse returns mdns.ErrClosed, as does whatever is
// asked after.
func (m *MDNS) Close() error {
	if err := m.peer.Close(); err != nil {
		return fmt.Errorf("closing Multicast DNS: %w", err)
	}

	return nil
}

// bridged is the interfaces the bridge is announced and looked for on.
func (m *MDNS) bridged() func(mdns.Interface) bool {
	if m.cfg.Bridge != "" {
		return mdns.On(m.cfg.Bridge)
	}

	return reachable
}

// reachable says whether an interface is one a controller could reach
// the host by: not one a container or a virtual machine runtime made,
// and not a point-to-point one.
func reachable(candidate mdns.Interface) bool {
	if made(candidate.Name) {
		return false
	}

	// The network says an interface's name and number, and not what it
	// is: the host does. One the host does not have under that name is a
	// network of another making, with no such interfaces.
	known, err := net.InterfaceByIndex(candidate.Index)
	if err != nil || known == nil {
		return true
	}

	return known.Name != candidate.Name || known.Flags&net.FlagPointToPoint == 0
}

// made says whether an interface is one a container or a virtual
// machine runtime made, by its name: it is up, carries multicast, and
// has addresses nothing on the network reaches.
func made(name string) bool {
	name = strings.ToLower(name)

	for _, runtime := range []string{
		"docker", "br-", "veth", "hassio", "virbr", "cni", "flannel", "cali", "kube", "podman",
	} {
		if strings.HasPrefix(name, runtime) {
			return true
		}
	}

	return false
}

// links is a network that remembers the addresses of its links: which
// of an interface's addresses a controller can reach the host at
// depends on the others it has.
type links struct {
	inner transport.Network

	mu    sync.Mutex
	addrs map[transport.Link][]netip.Prefix
}

// Watch is transport.Network's.
func (l *links) Watch(ctx context.Context, handle func(transport.Event)) error {
	//nolint:wrapcheck // the network's own word, handed on as it is
	return l.inner.Watch(ctx, func(event transport.Event) {
		if event.Link != nil {
			l.mu.Lock()

			if event.Change == transport.Down {
				delete(l.addrs, event.Link)
			} else {
				l.addrs[event.Link] = event.Link.Addrs()
			}

			l.mu.Unlock()
		}

		handle(event)
	})
}

// reaches says whether the host is announced at an address of an
// interface: at its routable ones, and at an IPv6 link-local one only
// when the interface has no routable IPv6 address. A controller was
// seen to go through the addresses it is given in their order, and to
// give up at one it cannot reach.
func (l *links) reaches(announced mdns.Interface, addr netip.Addr) bool {
	addr = addr.Unmap()

	switch {
	case addr.IsLoopback(), addr.IsUnspecified(), addr.Is4() && addr.IsLinkLocalUnicast():
		return false
	case addr.Is4(), !addr.IsLinkLocalUnicast():
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for link, held := range l.addrs {
		if link.Interface() != announced {
			continue
		}

		for _, prefix := range held {
			other := prefix.Addr().Unmap()
			if other.Is6() && !other.IsLinkLocalUnicast() && !other.IsLoopback() {
				return false
			}
		}
	}

	return true
}
