# matterialize

A daemon that makes the devices of a home that do not speak
[Matter](https://csa-iot.org/all-solutions/matter/) into Matter devices: one Matter
bridge, behind which each maker's devices appear to any Matter controller that is
invited to it, whatever its ecosystem, several at once.

The daemon is built, with two plugins: [Lutron Caséta](#lutron) and [Wemo](#wemo).

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

It is built on [go-matter](https://github.com/duncanite/go-matter), which is the
vocabulary of devices and the bridge itself.
