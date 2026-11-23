// Package dyson is the plugin for Dyson's purifiers of two generations,
// the Pure Cool Link and the Pure Hot+Cool: every one that announces
// itself on the local network and whose credential the plugin holds,
// written into the home as an air purifier and kept as it says it is.
//
// Its settings, every one of which may be left out:
//
//	dyson:
//	  # How long before a purifier that does not answer is reached for
//	  # again. Left out, ten seconds.
//	  retry: 10s
//	  # How often each purifier is asked what its sensors read. Left out,
//	  # thirty seconds.
//	  every: 30s
//
// Nothing in the settings names a purifier, or where one is. A purifier
// announces itself over Multicast DNS, with its serial and its product
// type in its name; the plugin hears it through the daemon's part in
// Multicast DNS, where the daemon's configuration has plugins listen,
// and dials it at the address it announces.
//
// An announcement is anyone's to make, and proves nothing: whatever
// announces itself under a purifier's serial is dialled, and offered
// that purifier's credential, which a purifier's own protocol sends as
// it is. What opens a purifier is that credential, which is not on the
// network until then: the
// plugin reads it from its store, under the purifier's serial, the
// string go-dyson's "dyson account" prints as DYSON_CREDENTIAL beside
// that serial. A purifier whose credential is not there is found, said
// in the log, and not served: put the credential there and it is, with
// no restart. A serial is whatever the network says, and one the store
// would not keep anything under is left alone.
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
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/forkcloser/go-mdns"

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

// errNowhere is a purifier that answered at none of the places it
// announces.
var errNowhere = errors.New("it answers nowhere it says it is")

// errNoCredential is a purifier the store holds no credential for.
var errNoCredential = errors.New("the store holds no credential for it")

// stored is the names a store takes, which a purifier's serial is used
// as.
var stored = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// settings is the plugin's, as the configuration writes them.
type settings struct {
	Retry string `yaml:"retry"`
	Every string `yaml:"every"`
}

// purifier is one purifier, as it announces itself: who it says it is,
// and where.
type purifier struct {
	Serial string
	Type   string
	At     []netip.AddrPort
}

// served is the plugin, made.
type served struct {
	store plugin.Store
	mdns  plugin.MDNS
	retry time.Duration
	every time.Duration

	mu sync.Mutex
	// dialled is each purifier as it was last dialled, which a device's
	// commands go to: a connection ends, and the device stays.
	dialled map[string]*dyson.Purifier
}

// followed is one purifier being kept in the home, where it was last
// said to be.
type followed struct {
	at   []netip.AddrPort
	stop context.CancelFunc
	done chan struct{}
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

	return &served{
		store: given.Store, mdns: given.MDNS, retry: retry, every: every,
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

// Run is Plugin's. It ends with ctx, and sooner only when the network
// can be browsed no more: a purifier that cannot be reached, or that
// goes, is unreachable in the home until it is there again.
func (s *served) Run(ctx context.Context, home *matter.Home) error {
	running, stop := context.WithCancel(ctx)
	following := map[string]*followed{}

	defer func() {
		stop()

		for _, one := range following {
			<-one.done
		}
	}()

	// left is why a purifier that announces itself is left alone, for as
	// long as it is: said once, not each time it is told of.
	left := map[string]string{}

	err := dyson.Find(running, lent{s.mdns}, func(found dyson.Found, lost bool) {
		s.told(running, home, following, left, found, lost)
	})

	if ctx.Err() != nil {
		// Stopped: what the browsing ended with is not the plugin's to say.
		return nil
	}

	return fmt.Errorf("finding Dyson purifiers: %w", err)
}

// told is what the network says of one purifier: that it is there, and
// where, or that it is gone. One that is new, or that is somewhere
// else, is followed from where it now is; one that is gone is followed
// no more, and stays in the home, unreachable.
func (s *served) told(
	ctx context.Context, home *matter.Home, following map[string]*followed, left map[string]string,
	found dyson.Found, lost bool,
) {
	was, known := following[found.Serial]

	// Told of again where it already is: nothing of it moved. An
	// announcement says where a purifier is a moment after it says who,
	// and until then there is nowhere to dial.
	if !lost && (len(found.Addresses) == 0 || known && slices.Equal(was.at, found.Addresses)) {
		return
	}

	if known {
		was.stop()
		<-was.done
		delete(following, found.Serial)
	}

	if lost {
		delete(left, found.Serial)

		return
	}

	if why := unserved(found); why != "" {
		if left[found.Serial] != why {
			slog.InfoContext(ctx, "a Dyson purifier is found, and left alone", "type", found.Type, "why", why)
		}

		left[found.Serial] = why

		return
	}

	following[found.Serial] = s.start(ctx, home, purifier{
		Serial: found.Serial, Type: found.Type, At: slices.Clone(found.Addresses),
	})
}

// unserved is why a purifier that announces itself is not one the
// plugin serves, and nothing for one it does. Its serial is whatever
// the network says: one the store would keep nothing under is no name
// to look a credential up by, and none to name a device with.
func unserved(found dyson.Found) string {
	switch {
	case !stored.MatchString(found.Serial):
		return "its serial is no name the store keeps a credential under"
	case modelOf(found.Type) == "":
		return "it is of no product type this plugin serves"
	default:
		return ""
	}
}

// start follows a purifier on a goroutine of its own, until it is
// stopped.
func (s *served) start(ctx context.Context, home *matter.Home, one purifier) *followed {
	following, stop := context.WithCancel(ctx)
	kept := &followed{at: one.At, stop: stop, done: make(chan struct{})}

	go func() {
		defer close(kept.done)
		defer stop()

		s.follow(following, home, one)
	}()

	return kept
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

	identity := dyson.Identity{
		Serial: one.Serial, Type: one.Type, Credential: strings.TrimSpace(string(credential)),
	}

	// The first of the places it announces that answers.
	var opened *dyson.Purifier

	for _, place := range one.At {
		dialled, failed := dyson.Dial(ctx, place.String(), identity)
		if failed == nil {
			opened = dialled

			break
		}

		err = errors.Join(err, fmt.Errorf("dialling it at %s: %w", place, failed))
	}

	if opened == nil {
		return errors.Join(errNowhere, err)
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

// lent is the daemon's Multicast DNS as go-dyson asks for the means of
// finding purifiers.
type lent struct {
	mdns plugin.MDNS
}

func (l lent) Browse(ctx context.Context, service string, handle func(dyson.Advertisement, bool)) error {
	return l.mdns.Browse(ctx, mdns.Query{Type: service}, func(change mdns.Change, heard mdns.Instance) {
		addresses := make([]netip.AddrPort, 0, len(heard.Addrs))
		for _, at := range heard.Addrs {
			addresses = append(addresses, netip.AddrPortFrom(at, heard.Port))
		}

		handle(dyson.Advertisement{Instance: heard.Name, Addresses: addresses}, change == mdns.Lost)
	})
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
