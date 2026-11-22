// Package dyson is the plugin for Dyson's purifiers of two generations,
// the Pure Cool Link and the Pure Hot+Cool: each one the settings name,
// written into the home as an air purifier and kept as it says it is.
//
// Its settings:
//
//	dyson:
//	  # How long before a purifier that does not answer is reached for
//	  # again. Left out, ten seconds.
//	  retry: 10s
//	  # How often each purifier is asked what its sensors read. Left out,
//	  # thirty seconds.
//	  every: 30s
//	  purifiers:
//	    - serial: NN2-US-HKA0000A
//	      # Its product type: 475 a Pure Cool Link, 527 a Pure Hot+Cool.
//	      type: "475"
//	      # Its address, with or without a port.
//	      host: 192.0.2.30
//	      # What a controller shows for it. Left out, the model's name.
//	      name: Bedroom purifier
//
// A purifier is opened by a credential of its own, which is not on the
// network. The plugin reads it from its store, under the purifier's
// serial: the string go-dyson's "dyson account" prints as
// DYSON_CREDENTIAL. A purifier whose credential is not there is said in
// the log and reached for again: put the credential there and it is
// served, with no restart.
//
// What a purifier reads of the air is bridged where it has a unit:
// the temperature, the humidity, and for a Hot+Cool the particles by
// weight. How clean the air is, good to extremely poor, is the plugin's
// own reading of the fine particles, by the breakpoints the United
// States' Environmental Protection Agency has for a day's average of
// PM2.5, as revised in 2024: good to 9.0 micrograms a cubic metre, fair
// to 35.4, moderate to 55.4, poor to 125.4, very poor to 225.4. A
// purifier's indices, of volatile compounds, of nitrogen dioxide, and a
// Link's of particles, have no unit and no scale that is known, and are
// not bridged.
package dyson

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-matter"

	"github.com/duncanite/matterialize/plugin"
)

const (
	// again is how long the plugin waits before it reaches again for a
	// purifier that did not answer, and often how often it asks one what
	// its sensors read, when its settings do not say.
	again = 10 * time.Second
	often = 30 * time.Second

	// full is a hundred percent, and the last of them a filter has left.
	full = 100

	// unread is what a Hot+Cool was seen to say of its particles under
	// one name while it said 149 under another: a sensor that reads
	// nothing, not air that holds a milligram.
	unread = 999

	// filterWarning is the purifier's warning for a filter to be
	// replaced.
	filterWarning = "FLTR"
)

// The fine particles a cubic metre of air holds, in micrograms, up to
// which the air is each quality, and above the last of which it is
// extremely poor: the breakpoints of the United States' Environmental
// Protection Agency for a day's average of PM2.5, as revised in 2024.
const (
	goodTo     = 9.0
	fairTo     = 35.4
	moderateTo = 55.4
	poorTo     = 125.4
	veryPoorTo = 225.4
)

// errNoCredential is a purifier the store holds no credential for.
var errNoCredential = errors.New("the store holds no credential for it")

// stored is the names a store takes, which a purifier's serial is used
// as.
var stored = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// settings is the plugin's, as the configuration writes them.
type settings struct {
	Retry     string     `yaml:"retry"`
	Every     string     `yaml:"every"`
	Purifiers []purifier `yaml:"purifiers"`
}

// purifier is one purifier, as the configuration names it.
type purifier struct {
	Serial string `yaml:"serial"`
	Type   string `yaml:"type"`
	Host   string `yaml:"host"`
	Name   string `yaml:"name"`
}

// served is the plugin, made.
type served struct {
	store     plugin.Store
	retry     time.Duration
	every     time.Duration
	purifiers []purifier

	mu sync.Mutex
	// dialled is each purifier as it was last dialled, which a device's
	// commands go to: a connection ends, and the device stays.
	dialled map[string]*dyson.Purifier
}

// New makes the plugin from its settings: it is the Maker to enable
// under the name "dyson".
func New(given plugin.Settings) (plugin.Plugin, error) {
	var read settings

	if err := given.Decode(&read); err != nil {
		return nil, fmt.Errorf("its settings: %w", err)
	}

	retry, err := lasting("retry", read.Retry, again)
	if err != nil {
		return nil, err
	}

	every, err := lasting("every", read.Every, often)
	if err != nil {
		return nil, err
	}

	if len(read.Purifiers) == 0 {
		return nil, fmt.Errorf("%w: it names no purifier", plugin.ErrSettings)
	}

	named := map[string]bool{}

	for _, one := range read.Purifiers {
		switch {
		case one.Serial == "" || one.Type == "" || one.Host == "":
			return nil, fmt.Errorf("%w: a purifier has a serial, a type and a host, each", plugin.ErrSettings)
		case !stored.MatchString(one.Serial):
			return nil, fmt.Errorf(
				"%w: %q is no serial the store keeps a credential under",
				plugin.ErrSettings,
				one.Serial,
			)
		case modelOf(one.Type) == "":
			return nil, fmt.Errorf(
				"%w: %q is no product type this plugin serves; it serves %s, the Pure Cool Link, and %s, the Pure Hot+Cool",
				plugin.ErrSettings,
				one.Type,
				dyson.PureCoolLink,
				dyson.PureHotCool,
			)
		case named[one.Serial]:
			return nil, fmt.Errorf("%w: the purifier %s is named twice", plugin.ErrSettings, one.Serial)
		}

		named[one.Serial] = true
	}

	return &served{
		store: given.Store, retry: retry, every: every, purifiers: read.Purifiers,
		dialled: map[string]*dyson.Purifier{},
	}, nil
}

// lasting is a length of time a setting gives, or what it is when the
// setting is left out.
func lasting(setting, written string, otherwise time.Duration) (time.Duration, error) {
	if written == "" {
		return otherwise, nil
	}

	given, err := time.ParseDuration(written)
	if err != nil || given <= 0 {
		return 0, fmt.Errorf("%w: %s is a length of time, as 10s is, not %q", plugin.ErrSettings, setting, written)
	}

	return given, nil
}

// modelOf is what a product type is called, and nothing for one this
// plugin does not serve: the types go-dyson speaks.
func modelOf(product string) string {
	switch product {
	case dyson.PureCoolLink:
		return "Dyson Pure Cool Link"
	case dyson.PureHotCool, dyson.PureHotCool + "E", dyson.PureHotCool + "K", dyson.PureHotCool + "M":
		return "Dyson Pure Hot+Cool"
	default:
		return ""
	}
}

// weighs says whether a product type reads particles by weight, has two
// filters that say what is left of them, and may heat: the generation
// after the Link's.
func weighs(product string) bool { return product != dyson.PureCoolLink }

// Run is Plugin's. It ends with ctx and with nothing else: a purifier
// that cannot be reached, or that goes, is unreachable in the home until
// it answers again.
func (s *served) Run(ctx context.Context, home *matter.Home) error {
	var following sync.WaitGroup

	for _, one := range s.purifiers {
		following.Go(func() { s.follow(ctx, home, one) })
	}

	following.Wait()

	return nil
}

// follow keeps a purifier in the home for as long as ctx lasts: read
// while it answers, unreachable while it does not, and reached for
// again.
func (s *served) follow(ctx context.Context, home *matter.Home, one purifier) {
	ident := idOf(one.Serial)

	// why is why the purifier was last not there, for as long as it is
	// not: a purifier that is unplugged is said so once, not at every
	// try.
	var why string

	back := func() {
		if why != "" {
			slog.InfoContext(ctx, "a Dyson purifier is back", "purifier", one.Serial)
		}

		why = ""
	}

	for {
		err := s.connected(ctx, home, one, back)

		if _, put := home.Device(ident); put {
			_ = home.Report(ident, matter.ReachabilityChanged{Unreachable: true})
		}

		if ctx.Err() != nil {
			return
		}

		level := slog.LevelDebug
		if said := err.Error(); said != why {
			level, why = slog.LevelWarn, said
		}

		slog.Log(ctx, level, "a Dyson purifier is not there", "purifier", one.Serial, "error", err, "retry_in", s.retry)

		select {
		case <-ctx.Done():
			return
		case <-time.After(s.retry):
		}
	}
}

// connected is one connection to a purifier, from its dialling to its
// end: the home brought to what the purifier says, and kept so. It
// returns why the connection ended, or why there was none. back is
// called each time the purifier has said how it stands.
func (s *served) connected(ctx context.Context, home *matter.Home, one purifier, back func()) error {
	credential, err := s.store.Get(ctx, one.Serial)
	if errors.Is(err, plugin.ErrNotStored) {
		return errNoCredential
	}

	if err != nil {
		return fmt.Errorf("reading its credential from the store: %w", err)
	}

	opened, err := dyson.Dial(ctx, one.Host, dyson.Identity{
		Serial: one.Serial, Type: one.Type, Credential: strings.TrimSpace(string(credential)),
	})
	if err != nil {
		return fmt.Errorf("dialling it: %w", err)
	}

	defer func() { _ = opened.Close() }()

	s.mu.Lock()
	s.dialled[one.Serial] = opened
	s.mu.Unlock()

	return s.read(ctx, home, one, opened, back)
}

// read asks a purifier how it stands and what its sensors read, now and
// then every so often, and puts into the home what it says, asked or
// not. It returns why it read no more. back is called each time the
// purifier has said how it stands.
func (s *served) read(
	ctx context.Context, home *matter.Home, one purifier, opened *dyson.Purifier, back func(),
) error {
	reading, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	// The asking is beside the reading: a purifier is not known to say
	// what its sensors read unless it is asked.
	go func() {
		if err := s.asks(reading, opened); err != nil {
			stop(err)
		}
	}()

	hearing := heard{back: back}

	for {
		said, err := opened.Next(reading)

		switch {
		case err == nil:
			err = s.heard(ctx, home, one, &said, &hearing)
		case errors.Is(err, dyson.ErrAnswer) && reading.Err() == nil:
			// What the purifier said that is not read is passed over: the
			// next thing it says may be.
			slog.WarnContext(ctx, "a Dyson purifier said what is not read", "purifier", one.Serial, "error", err)

			continue
		default:
			if cause := context.Cause(reading); cause != nil && ctx.Err() == nil {
				err = cause
			}

			err = fmt.Errorf("reading it: %w", err)
		}

		// A purifier that cannot be placed ends the connection: a home
		// that stays as it was while the purifier goes on is worse than
		// one reached for again.
		if err != nil {
			return err
		}
	}
}

// asks has a purifier say how it stands and what its sensors read, now
// and then every so often, until ctx ends. It returns why it could ask
// no more, and nothing when ctx ended.
func (s *served) asks(ctx context.Context, opened *dyson.Purifier) error {
	for {
		if err := opened.Ask(ctx); err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // stopped: the asking ended with the reading, and has nothing to say
			}

			return fmt.Errorf("asking it how it stands: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.every):
		}
	}
}

// heard is what one connection to a purifier has heard of it so far.
type heard struct {
	// codes is the purifier's error and warning as it last said them.
	codes string
	// back is called each time the purifier has said how it stands.
	back func()
}

// heard puts into the home one thing a purifier said: how it stands, or
// what its sensors read. Its codes are said in the log when it has any
// and they changed.
func (s *served) heard(
	ctx context.Context, home *matter.Home, one purifier, said *dyson.Update, before *heard,
) error {
	if said.State != nil {
		codes := said.State.Error + " " + said.State.Warning

		// A purifier with no code says nothing to be read in a log.
		if codes != before.codes && codes != " " {
			slog.WarnContext(ctx, "a Dyson purifier's codes", "purifier", one.Serial,
				"error", said.State.Error, "warning", said.State.Warning)
		}

		before.codes = codes

		if err := s.stands(home, one, said.State); err != nil {
			return err
		}

		before.back()
	}

	if said.Environment != nil {
		return sensed(home, one, said.Environment)
	}

	return nil
}

// stands puts a purifier into the home as it says it stands, or, when it
// is there, says it reachable and as it stands now. The home tells no
// one of what changes nothing.
func (s *served) stands(home *matter.Home, one purifier, state *dyson.State) error {
	ident := idOf(one.Serial)

	held, had := home.Device(ident)
	if !had {
		if err := home.Put(s.describe(one, state)); err != nil {
			return fmt.Errorf("putting the purifier %s: %w", one.Serial, err)
		}

		return nil
	}

	told := []matter.Change{matter.ReachabilityChanged{Unreachable: false}, fanOf(state, held.FanControl.Speed)}

	for index, was := range held.Filters {
		now := filterOf(was.Kind, state, !weighs(one.Type))

		// Whether a filter says how much of it is left is part of what the
		// device is, which no report changes: a purifier whose filter
		// begins to say it, or says it no more, is put anew.
		if (now.Left == nil) != (was.Left == nil) {
			return s.anew(home, one, state, &held)
		}

		told = append(told, matter.FilterChanged{Filter: index, Left: now.Left, Replace: now.Replace})
	}

	if held.Heater != nil && state.Heat != nil {
		told = append(told, matter.HeaterChanged{
			On: state.Heat.On, Heating: state.Heat.Heating, Target: targetOf(state.Heat, held.Heater.Target),
		})
	}

	if err := home.Report(ident, told...); err != nil {
		return fmt.Errorf("saying how the purifier %s stands: %w", one.Serial, err)
	}

	return nil
}

// anew puts a purifier that is in the home again, as it now says it
// stands, with what the home held of it that its state does not say:
// what its sensors read, the speed its fan comes back to, and the
// temperature its heater heats to.
func (s *served) anew(home *matter.Home, one purifier, state *dyson.State, held *matter.Device) error {
	again := s.describe(one, state)

	again.FanControl.Speed = fanOf(state, held.FanControl.Speed).Speed
	again.Temperature, again.Humidity = held.Temperature, held.Humidity

	if again.AirQuality != nil && held.AirQuality != nil {
		again.AirQuality = held.AirQuality
	}

	if again.Heater != nil && held.Heater != nil {
		again.Heater.Target = targetOf(state.Heat, held.Heater.Target)
	}

	if err := home.Put(again); err != nil {
		return fmt.Errorf("putting the purifier %s anew: %w", one.Serial, err)
	}

	return nil
}

// sensed says in the home what a purifier's sensors read. One that is
// not in the home yet is said nothing of: its state comes first.
func sensed(home *matter.Home, one purifier, read *dyson.Environment) error {
	ident := idOf(one.Serial)

	held, had := home.Device(ident)
	if !had {
		return nil
	}

	told := []matter.Change{
		matter.TemperatureChanged{Celsius: read.Temperature},
		matter.HumidityChanged{Percent: whole(read.Humidity)},
	}

	if held.AirQuality != nil {
		fine, coarse := weighed(read.PM25), weighed(read.PM10)
		told = append(told, matter.AirQualityChanged{Quality: qualityOf(fine), PM25: fine, PM10: coarse})
	}

	if err := home.Report(ident, told...); err != nil {
		return fmt.Errorf("saying what the purifier %s reads: %w", one.Serial, err)
	}

	return nil
}

// describe is one purifier as a device of the vocabulary, as it first
// says it stands. Its commands go to the purifier as it was last
// dialled. What its sensors read is not known yet.
func (s *served) describe(one purifier, state *dyson.State) matter.Device {
	model := modelOf(one.Type)

	var described matter.Device

	described.ID, described.Kind = idOf(one.Serial), matter.AirPurifier
	described.Name, described.Vendor, described.Model, described.Serial = model, "Dyson", model, one.Serial

	if one.Name != "" {
		described.Name = one.Name
	}

	now := func() *dyson.Purifier {
		s.mu.Lock()
		defer s.mu.Unlock()

		return s.dialled[one.Serial]
	}

	stood := fanOf(state, 1)

	described.FanControl = &matter.FanControl{
		Mode: stood.Mode, Speed: stood.Speed, Running: stood.Running, Steps: dyson.MaxSpeed,
		Oscillating: &stood.Oscillating, Sleep: &stood.Sleep,
		SetMode: func(ctx context.Context, mode matter.FanMode) error {
			wanted, known := map[matter.FanMode]dyson.Mode{
				matter.FanOff: dyson.Off, matter.FanOn: dyson.Fan, matter.FanAuto: dyson.Auto,
			}[mode]
			if !known {
				return fmt.Errorf("%w: a purifier has no mode %d", matter.ErrInvalid, mode)
			}

			return translate(now().SetMode(ctx, wanted))
		},
		SetSpeed: func(ctx context.Context, speed matter.Level) error {
			step := int(math.Round(float64(speed) * dyson.MaxSpeed))

			return translate(now().SetSpeed(ctx, min(max(step, dyson.MinSpeed), dyson.MaxSpeed)))
		},
		SetOscillating: func(ctx context.Context, on bool) error { return translate(now().SetOscillation(ctx, on)) },
		SetSleep:       func(ctx context.Context, on bool) error { return translate(now().SetNightMode(ctx, on)) },
	}

	described.Temperature, described.Humidity = &matter.Temperature{}, &matter.Humidity{}
	described.Filters = []matter.Filter{filterOf(matter.HEPA, state, !weighs(one.Type))}

	if !weighs(one.Type) {
		return described
	}

	described.AirQuality = &matter.AirQuality{PM25: &matter.Particulates{}, PM10: &matter.Particulates{}}
	described.Filters = append(described.Filters, filterOf(matter.Carbon, state, false))

	if state.Heat != nil {
		described.Heater = &matter.Heater{
			On:        state.Heat.On,
			Heating:   state.Heat.Heating,
			Target:    targetOf(state.Heat, dyson.MinHeatTarget),
			Least:     dyson.MinHeatTarget,
			Most:      dyson.MaxHeatTarget,
			SetOn:     func(ctx context.Context, on bool) error { return translate(now().SetHeat(ctx, on)) },
			SetTarget: func(ctx context.Context, celsius float64) error { return translate(now().SetHeatTarget(ctx, celsius)) },
		}
	}

	return described
}

// fanOf is a purifier's fan as the vocabulary tells it. A purifier that
// chooses its own speed, or is off, says none: it keeps the one it had.
func fanOf(state *dyson.State, had matter.Level) matter.FanChanged {
	stands := matter.FanChanged{
		Mode: matter.FanOff, Speed: had, Running: state.Running,
		Oscillating: state.Oscillating, Sleep: state.NightMode,
	}

	switch state.Mode {
	case dyson.Fan:
		stands.Mode = matter.FanOn
	case dyson.Auto:
		stands.Mode = matter.FanAuto
	case dyson.Off:
	}

	if state.Speed >= dyson.MinSpeed && state.Speed <= dyson.MaxSpeed {
		stands.Speed = matter.Level(state.Speed) / dyson.MaxSpeed
	}

	return stands
}

// filterOf is a purifier's filter of a kind, as it says it stands. A
// Link's one filter counts the hours it has left, of a life that is not
// known: it says none of how much is left, and is due when it has no
// hour. Either is due when the purifier warns of its filter.
func filterOf(kind matter.FilterKind, state *dyson.State, hours bool) matter.Filter {
	warned := strings.Contains(state.Warning, filterWarning)

	if hours {
		return matter.Filter{Kind: kind, Replace: warned || state.FilterLife <= 0}
	}

	left := state.HEPA
	if kind == matter.Carbon {
		left = state.Carbon
	}

	if left == nil {
		return matter.Filter{Kind: kind, Replace: warned}
	}

	fraction := float64(min(max(*left, 0), full)) / full

	return matter.Filter{Kind: kind, Left: &fraction, Replace: warned || *left <= 0}
}

// targetOf is the temperature a heater heats to, and the one it had
// when it says none.
func targetOf(heat *dyson.Heat, had float64) float64 {
	if heat.Target == nil {
		return had
	}

	return min(max(*heat.Target, dyson.MinHeatTarget), dyson.MaxHeatTarget)
}

// weighed is particles by weight as a purifier reads them, and nothing
// for a sensor that reads nothing.
func weighed(read *int) *float64 {
	if read == nil || *read < 0 || *read >= unread {
		return nil
	}

	return whole(read)
}

// whole is a whole reading as the vocabulary takes it.
func whole(read *int) *float64 {
	if read == nil {
		return nil
	}

	value := float64(*read)

	return &value
}

// qualityOf is how clean the air is, by the fine particles it holds.
func qualityOf(fine *float64) matter.Quality {
	switch {
	case fine == nil:
		return matter.QualityUnknown
	case *fine <= goodTo:
		return matter.QualityGood
	case *fine <= fairTo:
		return matter.QualityFair
	case *fine <= moderateTo:
		return matter.QualityModerate
	case *fine <= poorTo:
		return matter.QualityPoor
	case *fine <= veryPoorTo:
		return matter.QualityVeryPoor
	default:
		return matter.QualityExtremelyPoor
	}
}

// idOf is a purifier's ID in the home: the plugin's name first, so that
// no other plugin's device has it.
func idOf(serial string) matter.ID { return matter.ID("dyson/" + serial) }

// translate puts go-dyson's errors in the vocabulary's.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, dyson.ErrArgument), errors.Is(err, dyson.ErrUnsupported):
		return fmt.Errorf("%w: %w", matter.ErrInvalid, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", matter.ErrUnreachable, err)
	default:
		return fmt.Errorf("%w: %w", matter.ErrFailed, err)
	}
}
