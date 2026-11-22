package lent_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/forkcloser/go-upnp/ssdp"
	"github.com/forkcloser/go-upnp/ssdp/ssdptest"

	"github.com/duncanite/matterialize/internal/lent"
)

const kind = "urn:example:device:thing:1"

// What is lent is on the network from the first Browse and not before,
// tells what is advertised there, and is closed for good.
func TestSSDP(t *testing.T) {
	t.Parallel()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "lo0", 1, netip.MustParsePrefix("127.0.0.1/8"))
	lan.Device(
		netip.MustParseAddr("127.0.0.1"),
		ssdptest.Advert{USN: "uuid:thing::" + kind, Type: kind, Location: "http://127.0.0.1:1/thing.xml"},
	)

	lending := lent.NewSSDP(ssdp.Config{Network: host})

	time.Sleep(100 * time.Millisecond)

	if asked := lan.Searches(); len(asked) != 0 {
		t.Fatalf("with no Browse, it searched: %q", asked)
	}

	found, ended := make(chan ssdp.Advertisement, 1), make(chan error, 1)

	go func() {
		ended <- lending.Browse(t.Context(), kind, func(change ssdp.Change, heard ssdp.Advertisement) {
			if change == ssdp.Found {
				found <- heard
			}
		})
	}()

	select {
	case heard := <-found:
		if heard.USN != "uuid:thing::"+kind {
			t.Fatalf("it found %+v", heard)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("it found nothing")
	}

	if err := lending.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-ended:
		if !errors.Is(err, ssdp.ErrClosed) {
			t.Fatalf("closed, the Browse under way ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("closed, the Browse under way did not end")
	}

	err := lending.Browse(t.Context(), kind, func(ssdp.Change, ssdp.Advertisement) {})
	if !errors.Is(err, ssdp.ErrClosed) {
		t.Fatalf("closed, a Browse ended with %v", err)
	}
}

// What was never browsed through opened nothing, and closes all the
// same.
func TestSSDPNeverUsed(t *testing.T) {
	t.Parallel()

	lending := lent.NewSSDP(ssdp.Config{Network: ssdptest.NewHost()})

	if err := lending.Close(); err != nil {
		t.Fatal(err)
	}

	err := lending.Browse(t.Context(), kind, func(ssdp.Change, ssdp.Advertisement) {})
	if !errors.Is(err, ssdp.ErrClosed) {
		t.Fatalf("closed, a Browse ended with %v", err)
	}
}
