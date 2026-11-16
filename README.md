# matterialize

A daemon that makes the devices of a home that do not speak
[Matter](https://csa-iot.org/all-solutions/matter/) into Matter devices: one Matter
bridge, behind which each maker's devices appear to any Matter controller that is
invited to it, whatever its ecosystem, several at once.

The daemon is built, and no plugin yet: it runs what its configuration enables, and this
build has nothing to enable.

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
  example:
    setting: value
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
- **A plugin that ends is the end of the daemon**, with why: half a home is not served
  in silence. What runs the daemon starts it again.

It is built on [go-matter](https://github.com/duncanite/go-matter), which is the
vocabulary of devices and the bridge itself.
