// Package selection is which devices of a home the bridge serves: the
// ones a list of patterns takes, and every one when there is no list.
package selection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/duncanite/go-matter"
)

// ErrPattern is a pattern that names nothing.
var ErrPattern = errors.New("not a pattern of devices")

// wildcard is what ends a pattern that takes every device whose ID begins
// with what comes before it.
const wildcard = "*"

// Check says whether patterns can be used: each a device's ID, or the
// beginning of one and "*". A list that is there and holds none is
// refused, since it serves nothing.
func Check(patterns []string) error {
	if patterns != nil && len(patterns) == 0 {
		return fmt.Errorf("%w: the list holds none, and would serve nothing", ErrPattern)
	}

	for _, pattern := range patterns {
		if pattern == "" || strings.Contains(strings.TrimSuffix(pattern, wildcard), wildcard) {
			return fmt.Errorf("%w: %q; it is an ID, or the beginning of one and %q", ErrPattern, pattern, wildcard)
		}
	}

	return nil
}

// Of is the devices of a source that patterns take, as a source: what
// the bridge serves of a home. With no patterns it is every device.
//
// A device that is found is said in the log, once, with whether it is
// served: what is on the network and left out is to be seen, not
// guessed.
func Of(source matter.Source, patterns []string) matter.Source {
	return &selected{source: source, patterns: patterns}
}

type selected struct {
	source   matter.Source
	patterns []string
}

// Devices is matter.Source's.
func (s *selected) Devices(ctx context.Context) ([]matter.Device, error) {
	every, err := s.source.Devices(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // the source's own word, handed on as it is
	}

	taken := every[:0]

	for _, device := range every {
		if s.takes(device.ID) {
			taken = append(taken, device)
		}
	}

	return taken, nil
}

// Watch is matter.Source's.
func (s *selected) Watch(ctx context.Context, handle func(matter.Event)) error {
	//nolint:wrapcheck // the source's own word, handed on as it is
	return s.source.Watch(ctx, func(event matter.Event) {
		served := s.takes(event.Device)

		if added, found := event.Change.(matter.DeviceAdded); found {
			slog.InfoContext(ctx, "a device is found",
				"device", string(event.Device), "name", added.Device.Name, "vendor", added.Device.Vendor,
				"served", served)
		}

		if served {
			handle(event)
		}
	})
}

// takes says whether a device is one the patterns take.
func (s *selected) takes(device matter.ID) bool {
	if s.patterns == nil {
		return true
	}

	for _, pattern := range s.patterns {
		if begins, open := strings.CutSuffix(pattern, wildcard); open && strings.HasPrefix(string(device), begins) {
			return true
		}

		if pattern == string(device) {
			return true
		}
	}

	return false
}
