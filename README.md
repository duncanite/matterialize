# matterialize

A daemon that makes the devices of a home that do not speak
[Matter](https://csa-iot.org/all-solutions/matter/) into Matter devices: one Matter
bridge, behind which each maker's devices appear to any Matter controller that is
invited to it, whatever its ecosystem, several at once.

Nothing is built yet. What it is to be:

- **One bridge, one setup code.** A controller that pairs with it sees every device
  behind it.
- **A plugin for each maker**, compiled in and enabled by configuration read at start.
  A plugin turns its maker's devices into Matter ones and does nothing else.
- **Configuration in a file, secrets beside it.** The file names the plugins and their
  settings and holds no secret: key material is read from a directory, passwords from the
  environment.
- **Its Matter identity kept on disk**, so that a controller paired with it stays paired
  across restarts.

It is built on [go-matter](https://github.com/duncanite/go-matter), which is the
vocabulary of devices and the bridge itself.
