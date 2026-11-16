# matterialize

A daemon that makes the devices of a home that do not speak
[Matter](https://csa-iot.org/all-solutions/matter/) into Matter devices: one Matter
bridge, behind which each maker's devices appear to any Matter controller that is
invited to it, whatever its ecosystem, several at once.

The daemon is built, with one plugin: [Lutron Caséta](#lutron).

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

# A directory of key material for the plugins. Left out when no plugin reads one.
secrets: /etc/matterialize/secrets

# The plugins that are enabled, by name, each with its own settings. A plugin that
# is not named does not run.
plugins:
  lutron:
    host: 192.0.2.20
```

A key the daemon or a plugin does not know is refused, with where it is: a misspelt key
that silently enables nothing is worse than a refusal.

**No secret is written in it.** A plugin reads key material from the directory of
secrets, by a name its settings give, and passwords from the environment.

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
    # The credentials that pairing with the bridge gave, by the name of their
    # directory among the secrets. Left out, "lutron".
    credentials: lutron
    # How long before a bridge that is not there is reached for again. Left
    # out, ten seconds.
    retry: 10s
```

The credentials are what [go-lutron](https://github.com/duncanite/go-lutron)'s `lutron
pair` writes: `key.pem`, `certificate.pem` and `root.pem` in one directory. The plugin
does not pair.

A device switched on comes back to the level it last had; one that was off when it was
first seen comes back to full, the bridge not saying where it stood.

**A Caséta bridge that goes away does not end the daemon.** Its devices are unreachable
to every controller, as Matter has a bridged device be, until the bridge answers again;
the other plugins go on. When it answers, its devices are as it then says: in the state
they have, the ones it no longer has gone, the ones it gained there. That is also when a
device added to the bridge or taken from it is seen: the bridge's list is read when the
connection is made, and not again while it lasts.

It is built on [go-matter](https://github.com/duncanite/go-matter), which is the
vocabulary of devices and the bridge itself.
