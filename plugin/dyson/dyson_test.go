package dyson_test

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	godyson "github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-matter"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/plugin"
	"github.com/duncanite/matterialize/plugin/dyson"
)

// The fakes as the home names them.
const (
	link    matter.ID = "dyson/" + dysontest.Serial
	hotCool matter.ID = "dyson/" + dysontest.HotCoolSerial
)

// What the fakes are sent and say, by the purifier's own names.
const (
	sensors = "ENVIRONMENTAL-CURRENT-SENSOR-DATA"
	asking  = "REQUEST-PRODUCT-ENVIRONMENT-CURRENT-SENSOR-DATA"
)

// memory is a store of a test: what a plugin keeps, in memory.
type memory struct {
	mu   sync.Mutex
	held map[string][]byte
}

func (m *memory) Get(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, kept := m.held[name]
	if !kept {
		return nil, plugin.ErrNotStored
	}

	return value, nil
}

func (m *memory) Put(_ context.Context, name string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.held == nil {
		m.held = map[string][]byte{}
	}

	m.held[name] = value

	return nil
}

// credential is what opens a purifier with that sticker, as an operator
// puts it in the store: with the line's end a command printed after it.
func credential(t *testing.T, ssid, password string) []byte {
	t.Helper()

	identity, err := godyson.FromSticker(ssid, password)
	if err != nil {
		t.Fatal(err)
	}

	return []byte(identity.Credential + "\n")
}

// keeping is a store that holds the credentials of both fakes.
func keeping(t *testing.T) *memory {
	t.Helper()

	store := &memory{}

	_ = store.Put(t.Context(), dysontest.Serial, credential(t, dysontest.SSID, dysontest.WiFiPassword))
	_ = store.Put(
		t.Context(), dysontest.HotCoolSerial, credential(t, dysontest.HotCoolSSID, dysontest.HotCoolWiFiPassword),
	)

	return store
}

// settings is what the daemon hands the plugin: the settings written,
// and its store.
func settings(t *testing.T, store plugin.Store, written string) plugin.Settings {
	t.Helper()

	read, err := config.Parse([]byte("bridge: {name: Test, state: /state}\nplugins:\n  dyson: " + written + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	node := read.Plugins["dyson"]

	return plugin.Settings{Decode: config.Settings(&node), Store: store}
}

// aLink and aHotCool are the settings that name a fake of each model,
// where it is.
func aLink(fake *dysontest.Purifier) string {
	return `{serial: ` + dysontest.Serial + `, type: "475", host: "` + fake.Addr() + `"}`
}

func aHotCool(fake *dysontest.Purifier) string {
	return `{serial: ` + dysontest.HotCoolSerial + `, type: "527", host: "` + fake.Addr() + `", name: Den}`
}

// running is the plugin under way against a home.
type running struct {
	home  matter.Home
	ended chan error
	stop  context.CancelFunc
}

// run makes the plugin and runs it until the test ends, or until it is
// stopped.
func run(t *testing.T, store plugin.Store, written string) *running {
	t.Helper()

	made, err := dyson.New(settings(t, store, written))
	if err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(t.Context())
	started := &running{ended: make(chan error, 1), stop: stop}

	go func() { started.ended <- made.Run(ctx, &started.home) }()

	t.Cleanup(func() {
		stop()

		if err := <-started.ended; err != nil {
			t.Errorf("stopped, the plugin ended with %v", err)
		}
	})

	return started
}

// follows runs the plugin for one fake, asked what it reads once an
// hour: what a test has the fake say of its sensors stays said.
func follows(t *testing.T, purifier string) *running {
	t.Helper()

	return run(t, keeping(t), "{every: 1h, purifiers: ["+purifier+"]}")
}

// eventually waits for the home to hold what a test expects of a
// purifier.
func eventually(t *testing.T, home *matter.Home, id matter.ID, what string, holds func(now matter.Device) bool) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		now, there := home.Device(id)
		if there && holds(now) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s: the home holds %+v (there: %t)", what, now, there)
		}
	}
}

// read waits for the purifier's sensors to have been read once: the
// temperature is known.
func read(t *testing.T, started *running, id matter.ID) matter.Device {
	t.Helper()

	eventually(t, &started.home, id, "the sensors read", func(now matter.Device) bool {
		return now.Temperature != nil && now.Temperature.Celsius != nil
	})

	now, _ := started.home.Device(id)

	return now
}

// sent waits for the fake to have been sent a field at a value, in a
// request to set its state.
func sent(t *testing.T, fake *dysontest.Purifier, what, field, value string) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		received := fake.Received()
		for _, message := range received {
			if message.Message == "STATE-SET" && message.Data[field] == value {
				return
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s: the purifier was never sent %s as %s, among %+v", what, field, value, received)
		}
	}
}

// near says two readings are the same, to what a float keeps of them.
func near(read *float64, want float64) bool {
	return read != nil && math.Abs(*read-want) < 1e-6
}

// A Pure Cool Link the settings name is an air purifier of the home:
// its fan as it stands, the temperature and the humidity it reads, one
// filter that does not say how much of it is left, and nothing of the
// air's quality, which it gives no unit for, nor a heater, which it has
// none of.
func TestALink(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	now := read(t, follows(t, aLink(fake)), link)

	if now.Kind != matter.AirPurifier || now.Name != "Dyson Pure Cool Link" || now.Vendor != "Dyson" ||
		now.Model != "Dyson Pure Cool Link" || now.Serial != dysontest.Serial {
		t.Errorf("the purifier is described as %+v", now.Info)
	}

	fan := now.FanControl
	if fan == nil || fan.Mode != matter.FanOn || fan.Speed != 0.4 || !fan.Running || fan.Steps != 10 ||
		fan.Oscillating == nil || !*fan.Oscillating || fan.Sleep == nil || *fan.Sleep {
		t.Errorf("its fan stands as %+v", fan)
	}

	if !near(now.Temperature.Celsius, 24.55) || now.Humidity == nil || !near(now.Humidity.Percent, 58) {
		t.Errorf("it reads %+v and %+v", now.Temperature, now.Humidity)
	}

	if len(now.Filters) != 1 || now.Filters[0] != (matter.Filter{Kind: matter.HEPA}) {
		t.Errorf("its filters are %+v", now.Filters)
	}

	if now.AirQuality != nil || now.Heater != nil || now.Unreachable {
		t.Errorf("it has an air quality %+v, a heater %+v, unreachable %t", now.AirQuality, now.Heater, now.Unreachable)
	}
}

// A Pure Hot+Cool is one too, under the name its settings give, with
// more: the particles it weighs and what they make of the air, two
// filters that say how much of each is left, and a heater.
func TestAHotCool(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	started := follows(t, aHotCool(fake))

	eventually(t, &started.home, hotCool, "the particles weighed", func(now matter.Device) bool {
		return now.AirQuality != nil && now.AirQuality.PM25.Micrograms != nil
	})

	now, _ := started.home.Device(hotCool)

	if now.Kind != matter.AirPurifier || now.Name != "Den" || now.Model != "Dyson Pure Hot+Cool" ||
		now.Serial != dysontest.HotCoolSerial {
		t.Errorf("the purifier is described as %+v", now.Info)
	}

	if fan := now.FanControl; fan.Mode != matter.FanOn || fan.Speed != 0.5 || !fan.Running || !*fan.Oscillating {
		t.Errorf("its fan stands as %+v", fan)
	}

	if !near(now.Temperature.Celsius, 21.85) || !near(now.Humidity.Percent, 45) {
		t.Errorf("it reads %+v and %+v", now.Temperature.Celsius, now.Humidity.Percent)
	}

	air := now.AirQuality
	if air.Quality != matter.QualityGood || !near(air.PM25.Micrograms, 7) || !near(air.PM10.Micrograms, 10) {
		t.Errorf("the air is %d, with %v and %v", air.Quality, air.PM25.Micrograms, air.PM10.Micrograms)
	}

	if len(now.Filters) != 2 || now.Filters[0].Kind != matter.HEPA || !near(now.Filters[0].Left, 0.95) ||
		now.Filters[1].Kind != matter.Carbon || !near(now.Filters[1].Left, 0.8) ||
		now.Filters[0].Replace || now.Filters[1].Replace {
		t.Errorf("its filters are %+v", now.Filters)
	}

	heater := now.Heater
	if heater == nil || heater.On || heater.Heating || math.Abs(heater.Target-20.8) > 1e-6 ||
		heater.Least != godyson.MinHeatTarget || heater.Most != godyson.MaxHeatTarget {
		t.Errorf("its heater is %+v", heater)
	}
}

// What the home asks of a purifier's fan is asked of the purifier, and
// the home holds what the purifier then says.
func TestTheFanIsSet(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	started := follows(t, aLink(fake))
	fan := read(t, started, link).FanControl
	ctx := t.Context()

	if err := fan.SetSpeed(ctx, 0.72); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "a speed of seven tenths", "fnsp", "0007")
	eventually(t, &started.home, link, "at seven tenths", func(now matter.Device) bool {
		return now.FanControl.Speed == 0.7
	})

	// The least a purifier turns at is its first speed.
	if err := fan.SetSpeed(ctx, 0.01); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "the least speed", "fnsp", "0001")

	if err := fan.SetOscillating(ctx, false); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "still", "oson", "OFF")
	eventually(t, &started.home, link, "still", func(now matter.Device) bool { return !*now.FanControl.Oscillating })

	if err := fan.SetSleep(ctx, true); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "asleep", "nmod", "ON")
	eventually(t, &started.home, link, "asleep", func(now matter.Device) bool { return *now.FanControl.Sleep })

	for mode, word := range map[matter.FanMode]string{
		matter.FanAuto: "AUTO", matter.FanOff: "OFF", matter.FanOn: "FAN",
	} {
		if err := fan.SetMode(ctx, mode); err != nil {
			t.Fatal(err)
		}

		sent(t, fake, "the mode "+word, "fmod", word)
		eventually(t, &started.home, link, "in the mode "+word, func(now matter.Device) bool {
			return now.FanControl.Mode == mode
		})
	}

	if err := fan.SetMode(ctx, matter.FanMode(9)); !errors.Is(err, matter.ErrInvalid) {
		t.Errorf("a mode that is none: %v", err)
	}
}

// What a purifier says of its fan, whoever set it, is what the home
// holds; one that chooses its own speed says none, and keeps the one it
// had.
func TestWhatAPurifierSays(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	started := follows(t, aLink(fake))
	read(t, started, link)

	fake.Set("fnsp", "0009")
	eventually(t, &started.home, link, "at nine tenths", func(now matter.Device) bool {
		return now.FanControl.Speed == 0.9
	})

	fake.Set("fnsp", "AUTO")
	fake.Set("fmod", "AUTO")
	eventually(t, &started.home, link, "left to itself", func(now matter.Device) bool {
		return now.FanControl.Mode == matter.FanAuto && !now.FanControl.Running && now.FanControl.Speed == 0.9
	})

	fake.Set("oson", "OFF")
	fake.Set("nmod", "ON")
	eventually(t, &started.home, link, "still and asleep", func(now matter.Device) bool {
		return !*now.FanControl.Oscillating && *now.FanControl.Sleep
	})
}

// What a purifier's sensors read is what the home holds, and a sensor
// that reads nothing is one whose reading is not known.
func TestWhatAPurifierReads(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	started := follows(t, aLink(fake))
	read(t, started, link)

	fake.Say(sensors, "data", map[string]string{"tact": "2931", "hact": "0030", "pact": "0000", "vact": "0000"})
	eventually(t, &started.home, link, "cooler and drier", func(now matter.Device) bool {
		return near(now.Temperature.Celsius, 19.95) && near(now.Humidity.Percent, 30)
	})

	fake.Say(sensors, "data", map[string]string{"tact": "OFF", "hact": "OFF", "pact": "INIT", "vact": "INIT"})
	eventually(t, &started.home, link, "reading nothing", func(now matter.Device) bool {
		return now.Temperature.Celsius == nil && now.Humidity.Percent == nil
	})
}

// The fine particles a Hot+Cool weighs say how clean the air is, by the
// breakpoints of a day's average as revised in 2024; and a reading of
// 999 is a sensor that
// reads nothing, not air that holds a milligram.
func TestHowCleanTheAirIs(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	started := follows(t, aHotCool(fake))
	read(t, started, hotCool)

	for _, row := range []struct {
		fine int
		want matter.Quality
	}{
		{0, matter.QualityGood},
		{9, matter.QualityGood},
		{10, matter.QualityFair},
		{35, matter.QualityFair},
		{36, matter.QualityModerate},
		{55, matter.QualityModerate},
		{56, matter.QualityPoor},
		{125, matter.QualityPoor},
		{126, matter.QualityVeryPoor},
		{225, matter.QualityVeryPoor},
		{226, matter.QualityExtremelyPoor},
		{998, matter.QualityExtremelyPoor},
	} {
		fake.Say(sensors, "data", map[string]string{
			"tact": "2950", "hact": "0045", "p25r": strconv.Itoa(row.fine), "p10r": "0300",
		})
		eventually(t, &started.home, hotCool, strconv.Itoa(row.fine)+" micrograms", func(now matter.Device) bool {
			air := now.AirQuality

			// The coarse particles say nothing of how clean the air is.
			return air.Quality == row.want && near(air.PM25.Micrograms, float64(row.fine)) &&
				near(air.PM10.Micrograms, 300)
		})
	}

	fake.Say(sensors, "data", map[string]string{"tact": "2950", "hact": "0045", "p25r": "0999", "p10r": "0040"})
	eventually(t, &started.home, hotCool, "fine particles not read", func(now matter.Device) bool {
		air := now.AirQuality

		return air.Quality == matter.QualityUnknown && air.PM25.Micrograms == nil && near(air.PM10.Micrograms, 40)
	})

	fake.Say(sensors, "data", map[string]string{"tact": "2950", "hact": "0045", "p25r": "0020", "p10r": "0999"})
	eventually(t, &started.home, hotCool, "coarse particles not read", func(now matter.Device) bool {
		air := now.AirQuality

		return air.Quality == matter.QualityFair && near(air.PM25.Micrograms, 20) && air.PM10.Micrograms == nil
	})
}

// A Hot+Cool's filters say how much of each is left, and one with
// nothing left is to be replaced.
func TestTheFiltersOfAHotCool(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	started := follows(t, aHotCool(fake))
	read(t, started, hotCool)

	fake.Set("hflr", "0040")
	eventually(t, &started.home, hotCool, "the HEPA filter at two fifths", func(now matter.Device) bool {
		return near(now.Filters[0].Left, 0.4) && !now.Filters[0].Replace && near(now.Filters[1].Left, 0.8)
	})

	fake.Set("cflr", "0000")
	eventually(t, &started.home, hotCool, "the carbon filter spent", func(now matter.Device) bool {
		return near(now.Filters[1].Left, 0) && now.Filters[1].Replace && !now.Filters[0].Replace
	})
}

// A filter that says how much of it is left no more is one that does
// not say; the purifier is otherwise as it was, what its sensors read
// and its name included, and its commands go where they went.
func TestAFilterThatSaysNoMore(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	started := follows(t, aHotCool(fake))

	eventually(t, &started.home, hotCool, "the particles weighed", func(now matter.Device) bool {
		return now.AirQuality.PM25.Micrograms != nil
	})

	// A fan that says no speed of its own keeps the one it had, which
	// only the home holds.
	fake.Set("fnsp", "0008")
	fake.Set("fnsp", "AUTO")
	fake.Set("hflr", "INV")
	eventually(t, &started.home, hotCool, "the HEPA filter saying nothing", func(now matter.Device) bool {
		return now.Filters[0].Left == nil && now.Filters[0].Kind == matter.HEPA && near(now.Filters[1].Left, 0.8)
	})

	now, _ := started.home.Device(hotCool)

	if now.Name != "Den" || now.FanControl.Speed != 0.8 || !near(now.Temperature.Celsius, 21.85) ||
		!near(now.Humidity.Percent, 45) || !near(now.AirQuality.PM25.Micrograms, 7) ||
		now.AirQuality.Quality != matter.QualityGood || math.Abs(now.Heater.Target-20.8) > 1e-6 || now.Unreachable {
		t.Errorf("put anew, the purifier is %+v, its fan %+v, its air %+v, its heater %+v",
			now, now.FanControl, now.AirQuality, now.Heater)
	}

	if err := now.FanControl.SetSpeed(t.Context(), 0.3); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "a speed, once put anew", "fnsp", "0003")

	// And says it again.
	fake.Set("hflr", "0060")
	eventually(t, &started.home, hotCool, "the HEPA filter saying it again", func(now matter.Device) bool {
		return near(now.Filters[0].Left, 0.6)
	})
}

// A filter that did not say how much of it is left when the purifier
// was first read, and then says it, is one that says it.
func TestAFilterThatBeginsToSay(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	fake.Product["hflr"] = "INV"

	started := follows(t, aHotCool(fake))

	if now := read(t, started, hotCool); now.Filters[0].Left != nil || !near(now.Filters[1].Left, 0.8) {
		t.Fatalf("its filters are %+v", now.Filters)
	}

	fake.Set("hflr", "0050")
	eventually(t, &started.home, hotCool, "the HEPA filter at a half", func(now matter.Device) bool {
		return near(now.Filters[0].Left, 0.5) && !now.Filters[0].Replace
	})
}

// A Link's one filter is to be replaced when it has no hour left, and
// when the purifier warns of it.
func TestTheFilterOfALink(t *testing.T) {
	t.Parallel()

	for what, set := range map[string][2]string{"no hour left": {"filf", "0000"}, "warned of": {"wacd", "FLTR"}} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()

			fake := dysontest.New()
			t.Cleanup(fake.Close)

			started := follows(t, aLink(fake))

			if now := read(t, started, link); now.Filters[0].Replace {
				t.Fatal("a filter with hours left is to be replaced")
			}

			fake.Set(set[0], set[1])
			eventually(t, &started.home, link, "the filter due", func(now matter.Device) bool {
				return now.Filters[0].Replace && now.Filters[0].Left == nil
			})
		})
	}
}

// A Hot+Cool's heater is set from the home, and the home holds what the
// purifier then says of it.
func TestTheHeater(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	started := follows(t, aHotCool(fake))
	heater := read(t, started, hotCool).Heater
	ctx := t.Context()

	if err := heater.SetOn(ctx, true); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "to heat", "hmod", "HEAT")
	eventually(t, &started.home, hotCool, "heating", func(now matter.Device) bool {
		return now.Heater.On && now.Heater.Heating
	})

	if err := heater.SetTarget(ctx, 25); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "to 25 degrees", "hmax", "2982")
	eventually(t, &started.home, hotCool, "heating to 25 degrees", func(now matter.Device) bool {
		return math.Abs(now.Heater.Target-25) < 1e-6
	})

	if err := heater.SetTarget(ctx, 50); !errors.Is(err, matter.ErrInvalid) {
		t.Errorf("a target the heater does not take: %v", err)
	}

	if err := heater.SetOn(ctx, false); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "to heat no more", "hmod", "OFF")
	eventually(t, &started.home, hotCool, "heating no more", func(now matter.Device) bool {
		return !now.Heater.On && !now.Heater.Heating
	})
}

// A purifier is asked what its sensors read again and again, as often
// as the settings say: it is not known to say it by itself.
func TestAPurifierIsAskedAgain(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	started := run(t, keeping(t), "{every: 50ms, purifiers: ["+aLink(fake)+"]}")
	read(t, started, link)

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		asked := 0

		for _, message := range fake.Received() {
			if message.Message == asking {
				asked++
			}
		}

		if asked >= 4 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("the purifier was asked what it reads %d times", asked)
		}
	}
}

// A purifier whose credential is not in the store is not served, and is
// reached for again: with its credential put there, it is, and the
// daemon was not started again.
func TestAPurifierWithNoCredential(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	store := &memory{}
	started := run(t, store, "{retry: 50ms, purifiers: ["+aLink(fake)+"]}")

	time.Sleep(500 * time.Millisecond)

	if now, there := started.home.Device(link); there {
		t.Fatalf("with no credential, the home holds %+v", now)
	}

	if opened := fake.Hellos(); len(opened) != 0 {
		t.Fatalf("with no credential, the purifier was opened %d times", len(opened))
	}

	_ = store.Put(t.Context(), dysontest.Serial, credential(t, dysontest.SSID, dysontest.WiFiPassword))

	read(t, started, link)
}

// A purifier that refuses the credential the store holds is not served,
// and is reached for again.
func TestAPurifierThatRefuses(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	store := &memory{}
	_ = store.Put(t.Context(), dysontest.Serial, []byte("not its credential"))

	started := run(t, store, "{retry: 50ms, purifiers: ["+aLink(fake)+"]}")

	for deadline := time.Now().Add(10 * time.Second); len(fake.Hellos()) < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("a purifier that refuses was opened %d times", len(fake.Hellos()))
		}
	}

	if now, there := started.home.Device(link); there {
		t.Fatalf("refused, the home holds %+v", now)
	}
}

// A purifier that goes is unreachable in the home, and stays in it;
// reached again, it is reachable, as it then stands.
func TestAPurifierThatGoes(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	started := run(t, keeping(t), "{retry: 50ms, every: 1h, purifiers: ["+aLink(fake)+"]}")
	read(t, started, link)

	// Unreachable, then not: the purifier is dialled again a moment after
	// it went, and a look at the home would miss the time between.
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

	fake.Set("fnsp", "0002")
	eventually(t, &started.home, link, "at two tenths", func(now matter.Device) bool {
		return now.FanControl.Speed == 0.2
	})

	fake.Drop()

	for _, unreachable := range []bool{true, false} {
		select {
		case said := <-told:
			if said != unreachable {
				t.Fatalf("the purifier was said unreachable: %t, for %t", said, unreachable)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the purifier was never said unreachable: %t", unreachable)
		}
	}

	// Its commands go to the connection it has now.
	now, _ := started.home.Device(link)
	if err := now.FanControl.SetSpeed(t.Context(), 0.6); err != nil {
		t.Fatal(err)
	}

	sent(t, fake, "a speed, once back", "fnsp", "0006")
}

// The plugin ends with its context, and with nothing to say.
func TestThePluginStops(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	started := follows(t, aLink(fake))
	read(t, started, link)

	started.stop()

	select {
	case err := <-started.ended:
		if err != nil {
			t.Fatalf("stopped, the plugin ended with %v", err)
		}

		// The cleanup reads it too.
		started.ended <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("the plugin did not stop")
	}
}

// Settings that name a purifier whole are settings; ones that do not
// are refused.
func TestSettings(t *testing.T) {
	t.Parallel()

	const one = `{serial: A1, type: "475", host: 192.0.2.1}`

	for _, written := range []string{
		"{purifiers: [" + one + "]}",
		"{retry: 3s, every: 1m, purifiers: [" + one + `, {serial: B2, type: 527E, host: "192.0.2.2:1883", name: Den}]}`,
	} {
		if _, err := dyson.New(settings(t, nil, written)); err != nil {
			t.Errorf("settings %q: %v", written, err)
		}
	}

	for _, written := range []string{
		"", "{}", "{purifiers: []}",
		"{retry: soon, purifiers: [" + one + "]}", "{every: -1s, purifiers: [" + one + "]}",
		"{every: 0s, purifiers: [" + one + "]}",
		`{purifiers: [{type: "475", host: 192.0.2.1}]}`,
		`{purifiers: [{serial: A1, host: 192.0.2.1}]}`,
		`{purifiers: [{serial: A1, type: "475"}]}`,
		`{purifiers: [{serial: A1, type: "520", host: 192.0.2.1}]}`,
		`{purifiers: [{serial: A1, type: "527Z", host: 192.0.2.1}]}`,
		`{purifiers: [{serial: ../A1, type: "475", host: 192.0.2.1}]}`,
		`{purifiers: [{serial: A 1, type: "475", host: 192.0.2.1}]}`,
		"{purifiers: [" + one + ", " + one + "]}",
	} {
		if _, err := dyson.New(settings(t, nil, written)); !errors.Is(err, plugin.ErrSettings) {
			t.Errorf("settings %q: %v", written, err)
		}
	}

	for _, written := range []string{"{host: 192.0.2.1}", "{purifiers: [{serial: A1, port: 1883}]}"} {
		if _, err := dyson.New(settings(t, nil, written)); !errors.Is(err, config.ErrInvalid) {
			t.Errorf("settings %q: %v", written, err)
		}
	}
}
