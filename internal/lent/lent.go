// Package lent is what the daemon has one of and lends its plugins: its
// part in SSDP.
package lent

import (
	"context"
	"fmt"
	"sync"

	"github.com/forkcloser/go-upnp/ssdp"
)

// SSDP is the daemon's part in SSDP, a plugin.SSDP: one ssdp.Browser,
// put on the network when a plugin first browses, so that a daemon no
// plugin of which browses opens nothing.
type SSDP struct {
	cfg ssdp.Config

	mu      sync.Mutex
	browser *ssdp.Browser
	closed  bool
}

// NewSSDP makes the daemon's part in SSDP, and opens nothing.
func NewSSDP(cfg ssdp.Config) *SSDP {
	return &SSDP{cfg: cfg}
}

// Browse is plugin.SSDP's.
func (s *SSDP) Browse(ctx context.Context, target string, handle func(ssdp.Change, ssdp.Advertisement)) error {
	browser, err := s.open(ctx)
	if err != nil {
		return err
	}

	//nolint:wrapcheck // the Browser's own word, handed on as it is
	return browser.Browse(ctx, target, handle)
}

// Close takes the daemon off SSDP, when it was on: every Browse returns
// ssdp.ErrClosed, as does every one that comes after.
func (s *SSDP) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true

	if s.browser == nil {
		return nil
	}

	if err := s.browser.Close(); err != nil {
		return fmt.Errorf("closing SSDP: %w", err)
	}

	return nil
}

func (s *SSDP) open(ctx context.Context) (*ssdp.Browser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ssdp.ErrClosed
	}

	if s.browser == nil {
		opened, err := ssdp.Open(ctx, s.cfg)
		if err != nil {
			return nil, fmt.Errorf("opening SSDP: %w", err)
		}

		s.browser = opened
	}

	return s.browser, nil
}
