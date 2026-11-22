package selection_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/duncanite/go-matter"
	"github.com/duncanite/go-matter/mattertest"

	"github.com/duncanite/matterialize/internal/selection"
)

// home is a home with a lamp of one maker and two plugs of another.
func home(t *testing.T) *matter.Home {
	t.Helper()

	var made matter.Home

	for _, id := range []matter.ID{"lutron/zone/7", "wemo/A", "wemo/B"} {
		mattertest.Light(&made, id)
	}

	return &made
}

// ids is the IDs of what a source holds.
func ids(t *testing.T, source matter.Source) []matter.ID {
	t.Helper()

	devices, err := source.Devices(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	held := make([]matter.ID, 0, len(devices))
	for _, device := range devices {
		held = append(held, device.ID)
	}

	return held
}

// What a list takes is what is served: a device by its ID, every device
// of a beginning with "*", and every device when there is no list.
func TestWhatIsServed(t *testing.T) {
	t.Parallel()

	for what, row := range map[string]struct {
		patterns []string
		want     []matter.ID
	}{
		"no list":         {nil, []matter.ID{"lutron/zone/7", "wemo/A", "wemo/B"}},
		"everything":      {[]string{"*"}, []matter.ID{"lutron/zone/7", "wemo/A", "wemo/B"}},
		"one by its ID":   {[]string{"wemo/B"}, []matter.ID{"wemo/B"}},
		"a maker's":       {[]string{"wemo/*"}, []matter.ID{"wemo/A", "wemo/B"}},
		"a maker and one": {[]string{"lutron/*", "wemo/A"}, []matter.ID{"lutron/zone/7", "wemo/A"}},
		"none of these":   {[]string{"hue/*", "wemo"}, []matter.ID{}},
	} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()

			if err := selection.Check(row.patterns); err != nil {
				t.Fatal(err)
			}

			if held := ids(t, selection.Of(home(t), row.patterns)); !slices.Equal(held, row.want) {
				t.Errorf("it holds %v, for %v", held, row.want)
			}
		})
	}
}

// What happens to a device that is not served is told to no one, and
// what happens to one that is, is: as it is found, as it changes, and as
// it goes.
func TestWhatIsTold(t *testing.T) {
	t.Parallel()

	made := home(t)
	told := make(chan matter.Event, 16)

	ctx, stop := context.WithCancel(t.Context())
	ended := make(chan error, 1)

	go func() {
		ended <- selection.Of(made, []string{"wemo/B"}).Watch(ctx, func(event matter.Event) { told <- event })
	}()

	next := func(what string) matter.Event {
		t.Helper()

		select {
		case event := <-told:
			if event.Device != "wemo/B" {
				t.Fatalf("%s: told of %s", what, event.Device)
			}

			return event
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: told nothing", what)

			return matter.Event{}
		}
	}

	if _, added := next("as it stands").Change.(matter.DeviceAdded); !added {
		t.Fatal("the device that is served was not told as found")
	}

	// The one that is not served changes first: were it told, it would
	// come first.
	for _, id := range []matter.ID{"wemo/A", "wemo/B"} {
		if err := made.Report(id, matter.OnOffChanged{On: true}); err != nil {
			t.Fatal(err)
		}
	}

	if _, changed := next("switched").Change.(matter.OnOffChanged); !changed {
		t.Fatal("the device that is served was not told as switched")
	}

	made.Remove("wemo/A")
	made.Remove("wemo/B")

	if _, removed := next("gone").Change.(matter.DeviceRemoved); !removed {
		t.Fatal("the device that is served was not told as gone")
	}

	stop()

	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped, the watch ended with %v", err)
	}
}

// A pattern that names nothing is refused, and so is a list that holds
// none.
func TestPatternsThatAreNone(t *testing.T) {
	t.Parallel()

	for _, patterns := range [][]string{{}, {""}, {"wemo/*", ""}, {"*/zone"}, {"wemo/**"}, {"we*mo/*"}} {
		if err := selection.Check(patterns); !errors.Is(err, selection.ErrPattern) {
			t.Errorf("%q: %v", patterns, err)
		}
	}
}
