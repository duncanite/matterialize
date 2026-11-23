# matterialize

A daemon that makes the devices of a home that do not speak
[Matter](https://csa-iot.org/all-solutions/matter/) into Matter devices: one Matter
bridge, behind which each maker's devices appear to any Matter controller that is
invited to it, whatever its ecosystem, several at once.

The daemon is built, with three plugins: [Lutron Caséta](#lutron), [Wemo](#wemo) and
[Dyson](#dyson).

```bash
matterialize /etc/matterialize/config.yaml
```

## The configuration

```yaml
bridge:
  # What a controller shows for the bridge.
  name: Home
  # The directory the bridge keeps as its own: who paired with it, and which
  # device has which place. The same directory is the same bridge after a
  # restart; without it every controller has to be paired again.
  state: /var/lib/matterialize
  # The network interface it announces itself on. Left out, all of them.
  interface: en0

# The directory the daemon keeps in what its plugins give it to keep: the
# credentials a pairing gave. Left out when no plugin keeps anything.
store: /var/lib/matterialize/store

# Which devices the bridge serves, of those the plugins find: a device's ID, or
# the beginning of one and "*". Left out, every device is served.
serve:
  - lutron/*
  - wemo/Socket-1_0-221234K0100001

# The plugins that are enabled, by name, each with its own settings. A plugin that
# is not named does not run.
plugins:
  lutron:
    host: 192.0.2.20
```

A key the daemon or a plugin does not know is refused, with where it is: a misspelt key
that silently enables nothing is worse than a refusal.

**No secret is written in it.** What a plugin has to keep that is one, the credentials
a pairing gave, it gives the daemon, which keeps it in the store: one directory for
each plugin, of files only the daemon's user reads. A plugin opens no file itself.

## What it does

- **One bridge, one setup code.** A controller that pairs with it sees every device
  behind it. A bridge no controller holds yet is open to one for fifteen minutes from
  the start, and how to pair is written to standard output and nowhere else.
- **A plugin for each maker**, compiled in and enabled by the configuration, read once
  at the start. A plugin turns its maker's devices into Matter ones and does nothing
  else.
- **Every device found is said in the log**, once, with its ID, its name and whether
  it is served: a plugin goes on finding its maker's devices for as long as the daemon
  runs, and what is on the network and left out is to be seen, not guessed. The ID in
  the log is what `serve` takes. A list of what is served keeps a home to what was
  chosen; it does not prove a device is what it says it is.
- **A maker that goes away is its plugin's to wait for**: its devices are unreachable
  until it is back, and the rest of the home is served. A plugin ends only for what it
  cannot come back from, and that is the end of the daemon, with why.

## The plugins

### Lutron

The zones of one Caséta bridge. A dimmer is a light with a level, a switch a light
without, and one that is plugged in or outdoors a plug; each keeps the name, the room,
the model and the serial number the bridge has for it. Nothing else is served, whatever
level it has: a shade or a fan is not a lamp. A zone the bridge says no level of is left
out.

```yaml
plugins:
  lutron:
    # The Caséta bridge, by address or name; with a port, or LEAP's own.
    host: 192.0.2.20
    # Where the bridge takes a pairing, when that is not the same host on the
    # pairing port.
    pairing: 192.0.2.20:8083
    # How long before a bridge that is not there is reached for again. Left
    # out, ten seconds.
    retry: 10s
```

**The plugin pairs with the bridge by itself**, the first time it runs: it says so in
the log, and waits for the button on the back of the bridge to be pressed. What the
pairing gave is kept in the store as `lutron/credentials`: the key, the certificate the
bridge signed and the bridge's root, three PEM blocks in that order. Take the file away
and the plugin pairs again. Put there what
[go-lutron](https://github.com/duncanite/go-lutron)'s `lutron pair` wrote (`key.pem`,
`certificate.pem` and `root.pem`, one after the other) and it does not pair at all.

A bridge that no longer takes the pairing is not paired with again by itself: what
answers at an address is not always the bridge that was paired.

A device switched on comes back to the level it last had; one that was off when it was
first seen comes back to full, the bridge not saying where it stood.

**A Caséta bridge that goes away does not end the daemon.** Its devices are unreachable
to every controller, as Matter has a bridged device be, until the bridge answers again;
the other plugins go on. When it answers, its devices are as it then says: in the state
they have, the ones it no longer has gone, the ones it gained there. That is also when a
device added to the bridge or taken from it is seen: the bridge's list is read when the
connection is made, and not again while it lasts.

### Wemo

Belkin's Wi-Fi Wemo plugs: every one that advertises itself on the local network, each a
plug with the name, the model, the serial number and the firmware it says it has. None
is named in the configuration, and the plugin keeps nothing.

```yaml
plugins:
  wemo:
    # How long a plug is given to answer, and how long before one that did not
    # is reached for again. Left out, ten seconds.
    retry: 10s
    # The port plugs send their events to. Left out, any free one.
    port: 49200
```

With no setting to give, the plugin is enabled with nothing under its name: `wemo: {}`.

**The plugs call the daemon back.** A plug tells what it is at by sending it to this
host, at the host's address on the plug's own network and the port above; a firewall has
to let that in. A plug that cannot reach it there says nothing and is not served: the
log says which, and why. A plug on another network than any of the host's is not served
either.

The plugs are found over SSDP, which the daemon has one part in for every plugin: it is
on every interface of the host, from the first plugin that asks.

**A plug that leaves the network is unreachable, and stays in the home**; back, it is as
it then says. A plug that moved to another address or started again is taken up anew by
itself. A plug renamed keeps the name it had until the daemon is started again.

**A plug has no credential.** Whatever advertises itself as a plug on the local network,
and describes itself as that plug where it says it is, is served as one.

### Dyson

Dyson's purifiers of two generations, the Pure Cool Link and the Pure Hot+Cool: each one
the configuration names, an air purifier with its fan, what it reads of the air, its
filters and, for a Hot+Cool, its heater.

```yaml
plugins:
  dyson:
    # How long before a purifier that does not answer is reached for again. Left
    # out, ten seconds.
    retry: 10s
    # How often each purifier is asked what its sensors read. Left out, thirty
    # seconds.
    every: 30s
    purifiers:
      - serial: NN2-US-HKA0000A
        # Its product type: 475 a Pure Cool Link, 527 a Pure Hot+Cool.
        type: "475"
        # Its address, with or without a port.
        host: 192.0.2.30
        # What a controller shows for it. Left out, the model's name.
        name: Bedroom purifier
```

**A purifier is opened by a credential of its own**, which is not on the network and not
in the configuration. The plugin reads it from the store, as `dyson/` and the purifier's
serial: one line, the string [go-dyson](https://github.com/duncanite/go-dyson)'s
`dyson account` prints for that purifier as `DYSON_CREDENTIAL`. A purifier whose
credential is not there is said in the log and reached for again: put the file there and
the purifier is served, with no restart.

A purifier is at the address the configuration gives. Finding purifiers by themselves,
over mDNS, comes with the daemon lending its plugins mDNS as it lends them SSDP.

What is served of each:

- **The fan**: off, on at one of its ten speeds, or left to the purifier; whether it
  turns from side to side; and its night mode, as a fan's sleep mode.
- **The temperature and the humidity** it reads.
- **Its filters**, with whether each is to be replaced. A Hot+Cool's two, HEPA and
  carbon, say how much of each is left, for as long as the purifier says it. A Link's
  one counts hours of a life that is not known, and says only when it is due: with no
  hour left, or when the purifier warns.
- **For a Hot+Cool, the air**: the fine and the coarse particles it weighs, in micrograms
  a cubic metre, and how clean that makes the air, good to extremely poor. That last is
  the plugin's own reading of the fine particles, by the breakpoints the United States'
  Environmental Protection Agency has for a day's average of PM2.5, as revised in 2024:
  good to 9.0, fair to 35.4, moderate to 55.4, poor to 125.4, very poor to 225.4, and
  extremely poor above. A reading of 999 is taken for a sensor that reads nothing.
- **For a Hot+Cool, the heater**: set to heat or not, the temperature it heats to, and
  whether it heats now.

**Not served**: a purifier's indices of volatile compounds and of nitrogen dioxide, and
a Link's index of particles. They have no unit, and the scale of none is known; a number
that says nothing is not put before a controller. So a Link says nothing of how clean the
air is. The sleep timer is not served either: Matter has no word for it.

A purifier is not known to say what its sensors read unless it is asked, so it is asked
again and again; what its fan, its filters and its heater do, it says as it happens.

**A purifier that goes is unreachable, and stays in the home**; reached again, it is as
it then says. Why it is not there is said in the log once, and again when the reason
changes, not at every try.

It is built on [go-matter](https://github.com/duncanite/go-matter), which is the
vocabulary of devices and the bridge itself.
