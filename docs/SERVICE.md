# HFDesk Service Mode

HFDesk can run as a supervised background service instead of an interactive
process. The platform supervisor owns the lifecycle — systemd on Linux, launchd
on macOS, the Windows Service Control Manager on Windows — and HFDesk stays a
plain foreground process inside it.

The supervisor owns the lifecycle completely:

- HFDesk stays a foreground process: no self-daemonization, no PID file, no
  `nohup`, no shell wrapper, no Task Scheduler, and no startup-folder
  shortcut.
- The service is headless: it never opens a browser and never shows a tray
  icon.
- The supervisor starts the instance at boot of its scope and restarts it when
  it fails.

Service mode is a **separate command family**. `hfdesk` without a `service`
subcommand behaves exactly as before: same flags, same config precedence, same
browser launch. Nothing about an interactive install is relocated by a service
installation.

## Quick start

```bash
hfdesk service install          # per-user service on Linux/macOS
hfdesk service status           # where it is registered and what it binds
```

The dashboard is then served on <http://127.0.0.1:8080>.

## Prerequisites

- A HFDesk binary at a **stable, absolute path** (see
  [Upgrades](#upgrades)).
- The platform supervisor: `systemd` on Linux, `launchd` on macOS, the Service
  Control Manager on Windows.
- Administrator rights for a machine-wide (`--system`) installation, and for
  the machine-wide `start`, `stop` and `restart`. HFDesk never elevates
  itself.
- The **service account** of a machine-wide installation, already created (see
  [The identity of a machine-wide instance](#the-identity-of-a-machine-wide-instance)).
  A user-scope installation needs no account: it runs as the user itself.
- A writable state root for the chosen scope (see
  [Where the state lives](#where-the-state-lives)).

The interactive CLI keeps its existing config precedence: a config file next to
the launch/executable directory first, then the per-user config directory.
Service mode does not relocate existing interactive installs and does not
change where an interactive run reads its config from.

## Command surface

```
hfdesk service install   [--user|--system] [--listen HOST:PORT] [--config PATH] [--state-dir DIR] [--log-dir DIR] [--cache-dir DIR] [--hub-dir DIR] [--local-dir DIR] [--service-user ACCOUNT] [--executable PATH]
hfdesk service uninstall [--user|--system]
hfdesk service start     [--user|--system]
hfdesk service stop      [--user|--system]
hfdesk service restart   [--user|--system]
hfdesk service status    [--user|--system]
hfdesk service run       --config PATH --state-dir DIR --log-dir DIR [--listen HOST:PORT] [--cache-dir DIR] [--hub-dir DIR] [--local-dir DIR]
```

`service run` is the **internal host mode** the supervisor invokes. It is not
meant for interactive use: it requires a complete, absolute path model and it
never opens a browser.

### Install flags

| Flag | Meaning |
|---|---|
| `--user` | Per-user installation (the default on Linux and macOS). |
| `--system` | Machine-wide installation. Needs administrator rights. |
| `--listen HOST:PORT` | Bind address of the instance. Default `127.0.0.1:8080`. |
| `--config PATH` | Absolute path of the config file the instance owns. |
| `--state-dir DIR` | Absolute path of the state directory (jobs, history, mirror targets). |
| `--log-dir DIR` | Absolute path of the log directory. |
| `--cache-dir DIR` | Absolute path of the Hugging Face cache root. Without `--hub-dir`, the hub directory follows it: `<cache root>/hub`. |
| `--hub-dir DIR` | Absolute path of the Hugging Face hub cache directory. Given explicitly, it separates the hub from the cache root on purpose. |
| `--local-dir DIR` | Absolute path of the flat output directory. |
| `--service-user ACCOUNT` | Account a **machine-wide** instance runs as. Default: the service account of the platform. Rejected for the user scope. |
| `--executable PATH` | Absolute path of the binary the registration points at. Default: the binary that is invoked. |

The path flags override the default locations of the scope. Every path of an
installation must be absolute; a relative path is rejected instead of being
resolved against the working directory of the supervisor.

`--service-user` and `--executable` belong to `install` alone: they describe
the registration that is written, and a lifecycle command has no registration
to write them into. `--listen` is a flag of `install` and of the internal
`run` mode, because both of them bind the address of the instance.

`--executable` names the program of the registration when it differs from the
binary that is run. The path must be absolute, must exist, must be a regular
file and must be executable. Without it the **invoked** path is used, so a
symbolic link keeps its name in the registration (see
[Upgrades](#upgrades)); a bare name that comes from the search path of the
shell names no file, so the resolved path of the running binary is registered
instead.

`--service-user` names the account of a machine-wide instance (see
[The identity of a machine-wide instance](#the-identity-of-a-machine-wide-instance)).
It applies to the machine-wide scope only: a user-scope instance runs as the
user itself, so the option is rejected there instead of being ignored.

The cache root and the hub directory are **one layout**: the cache root holds
the friendly views (`models/`, `datasets/`), the hub directory holds the
blobs. `--cache-dir` alone therefore moves the whole pair — the registered
hub directory becomes the `hub` child (`<cache root>/hub`) of the registered
cache root, so a registration never names two different trees. `--hub-dir`
names the hub directory explicitly and separates the two locations on
purpose, the same independence `HF_HOME` and `HF_HUB_CACHE` have. The
summary of the installation reports both halves of the pair it registered.

A cache or output directory recorded in the registration is the **fallback**
of the instance: the config file the instance owns wins over it (see
[The effective cache root of an instance](#the-effective-cache-root-of-an-instance)).

`install` validates the installation before it writes anything: the
executable must exist, be a regular file and be executable, the bind address
must be usable, and the runtime paths must be absolute. A failed installation
restores the previous registration instead of leaving a half-written one
behind.

The installation creates the directories of the instance — the config
directory, the state directory and the log directory — with owner-only
permissions (`0700`). A directory that **already exists** keeps its
permissions **and its ownership**: the installation never tightens a
directory the user named (for example with `--state-dir`), because that
directory may be shared with something else, and it never takes a directory
over from the owner it already has. A machine-wide instance grants the
service account only the directories it created itself; a directory that
already existed has to be writable by that account already (see
[The identity of a machine-wide instance](#the-identity-of-a-machine-wide-instance)).
The running instance follows the same rule for its log directory and its log
file: it creates them owner-only when they are missing and never changes the
mode of a directory or of a file that already exists.

### Lifecycle flags

`uninstall`, `start`, `stop`, `restart` and `status` take the scope selector
only (`--user` or `--system`).

## Scopes

| | `--user` | `--system` |
|---|---|---|
| Linux | systemd **user** unit (`systemctl --user`) | machine-wide systemd unit |
| macOS | per-user **LaunchAgent** | machine-wide **LaunchDaemon** |
| Windows | not available | machine-wide: the Windows Service Control Manager |

`--user` is the default on Linux and macOS. Windows always uses the Windows
Service Control Manager, which registers machine-wide services only: there is
no `--user` mode there, and asking for one is rejected instead of silently
falling back to a machine-wide root.

A user-scope instance and a system-scope instance never share a state root.

## Where the state lives

| Scope | Config file | State root (jobs, history, mirror targets) | Log dir |
|---|---|---|---|
| user (Linux/macOS) | `<user config dir>/HFDesk/service/hfdesk.json` | `<user config dir>/HFDesk/service` | `<user config dir>/HFDesk/service/logs` |
| system (Linux/macOS) | `/etc/hfdesk/hfdesk.json` | `/var/lib/hfdesk` | `/var/log/hfdesk` |
| Windows (machine) | `%ProgramData%\HFDesk\hfdesk.json` | `%ProgramData%\HFDesk` | `%ProgramData%\HFDesk\logs` |

`<user config dir>` is the per-user HFDesk configuration directory of the
platform, for example `~/.config/HFDesk` on Linux and
`~/Library/Application Support/HFDesk` on macOS.

The state root holds the mutable runtime state of the instance: the job list,
the download history and the mirror targets. The config file is the settings
file of the instance and holds its credentials.

## The effective cache root of an instance

An instance has two sources for its storage locations: the registration it
was installed with and the config file it owns. The **config file of the
instance wins**:

- When the config file defines a `cache-dir`, that is the cache root the
  instance uses. The cache root recorded in the registration is the fallback
  for an instance whose config file does not define one.
- The same holds for the flat output directory (`local-dir`).
- The hub directory follows the cache root: it is the `hub` child
  (`<cache root>/hub`) of the cache root the config file of the instance
  defines. Without a cache root from the config file, the hub directory
  recorded in the registration is used, and a registration without one falls
  back to the `hub` child of the effective cache root.

By default the registration records the cache root of the scope: the
Hugging Face cache of the user (`HF_HOME`, otherwise `~/.cache/huggingface`)
for a user-scope instance, and `<state dir>/cache` for a machine-wide one
(`/var/lib/hfdesk/cache` on Linux and macOS, `%ProgramData%\HFDesk\cache` on
Windows). The registered hub directory is the `hub` child of the registered
cache root, unless a hub directory was given explicitly (see
[Install flags](#install-flags)).

**Consequence for the user:** changing the cache directory in the settings of
the service (`POST /api/settings`) is honoured by the next start of the
service, because those settings are written into the config file of the
instance and that file wins over the registration.

The interactive CLI is unchanged: there a `--cache-dir` flag still wins over
the config file, exactly like a `--local-dir` flag.

## Network exposure

**Service mode binds loopback by default** (`127.0.0.1:8080`), even though the
interactive server defaults to a wildcard address. A supervised instance has
nobody sitting in front of it, so it must not expose the dashboard to the
network by accident.

To reach the service from another machine, request the address explicitly:

```bash
hfdesk service install --listen 0.0.0.0:8080     # every interface
hfdesk service install --listen 192.0.2.10:8080   # one interface
```

An address with an empty host is rejected, because an empty host binds every
interface. `install` and `status` make a non-loopback bind visible, so an
exposed instance is never silent about it.

## Credentials

The Hugging Face token, the proxy password and the basic-auth password are
**never** written into the supervisor registration, the process command line,
or the environment of the unit/plist. A registration is readable by every user
of the machine, so it carries paths and the bind address only.

The credentials live in the config file the service owns, which HFDesk writes
with owner-only permissions (`0600` on Unix). In service mode the instance
reads the token from that config file; the `HF_TOKEN` environment variable is
deliberately ignored, so a supervised instance cannot silently pick up a
credential from an inherited environment.

**Practical consequence:** after `hfdesk service install`, set the token for
the service through the service's own web UI (`POST /api/settings`), which
persists it into the service-owned config file.

## Logs

Service mode writes its own log file into the log directory of the instance
with bounded rotation:

- `hfdesk.log` — the active log file
- `hfdesk.log.1`, `hfdesk.log.2`, `hfdesk.log.3` — the rotated generations

The active file is rotated before a write would exceed 2 MiB, and three
rotated generations are kept, so the on-disk size of the log stays bounded.
The instance creates its log directory and its log files owner-only (`0700` /
`0600` on Unix) when they are missing. It never changes the mode of a
directory or of a file that already exists: the log directory may be a
directory that existed before the installation and that belongs to someone
else, and the instance may only be able to write there. Tightening it needs
an ownership the installation never checked, and it would keep the instance
from starting at all.

The standard output and the standard error of the process also reach the
supervisor: the systemd journal on Linux. On macOS the property list sets
**no** `StandardOutPath` and **no** `StandardErrorPath`: launchd does not
rotate the files it owns, so they would grow without bound. launchd
therefore discards them, and the bounded `hfdesk.log` inside the log
directory of the instance is the real log sink on macOS.

## Lifecycle commands

| Command | Effect | A service that is not installed |
|---|---|---|
| `install` | Registers the service with the supervisor and enables it for the boot of the scope. A macOS LaunchAgent starts the instance as soon as it is registered (`RunAtLoad`); a systemd unit is only registered and enabled. The `State` line of the summary reports the state the supervisor reached. An existing registration is overwritten. | It creates the registration. |
| `uninstall` | Removes the **registration only**. | Clean success: the registration is already gone. |
| `start` | Starts the registered service. | **Error** that says the service is not installed. |
| `stop` | Stops the registered service. | Clean success: there is nothing running to stop. |
| `restart` | Stops and starts the registered service. | **Error** that says the service is not installed. |
| `status` | Reports scope, state, registration path, executable, bind address, runtime paths and the effective cache root. | Reports the state `not-installed` and exits with `1`. |

The **machine-wide scope** needs an **elevated shell** for `start`, `stop` and
`restart`, exactly as it needs one for `install` and `uninstall` (see
[Privileged operations](#privileged-operations)). The user scope needs no
elevation: the instance belongs to the user who runs the command.

**Uninstall never deletes user data.** It removes the registration and nothing
else: not the models, not the cache, not the config file, not the jobs or the
history, not the logs. Removing the state of an instance is a separate,
deliberate action.

The `Cache root` line of `status` and of `uninstall` is the **effective**
cache root of the instance — the one the instance actually uses (see
[The effective cache root of an instance](#the-effective-cache-root-of-an-instance)).
It is not necessarily the cache root that was recorded in the registration:
an instance whose own config file defines a `cache-dir` uses that one.

The summary of `install` reports the same pair: its `Cache root` and `Hub
dir` lines name the two directories the registered instance uses, so a reader
sees at once which tree the models of the instance go into.

The commands are idempotent where the wanted state is already reached:

- `uninstall` on a service that is not installed succeeds; the registration is
  already gone.
- `start` on a service that already runs succeeds.
- `stop` on a service that is already stopped succeeds, and so does a `stop` on
  a service that is not installed at all: there is nothing running to stop.
- `install` over an existing registration replaces it.

`start` and `restart` are the two commands that need a registration to work on:
on a service that is not installed they fail with a readable "not installed"
error.

## Exit codes

Every command of the service family ends with one of three exit codes:

| Code | Meaning |
|---|---|
| `0` | The operation succeeded. |
| `1` | The operation failed: the service is not installed, the elevation is missing, the supervisor refused the operation, or the installation or the removal failed. |
| `2` | The command line could not be understood: an unknown or a missing subcommand, an unknown flag, mutually exclusive scope selectors, a relative path, an unusable `--executable` or an unparsable `--listen`. |

`status` uses the same codes with a narrower meaning: it exits with `0` when the
state of the service is `running`, `stopped`, `starting` or `stopping`, and with
`1` when the state is `not-installed`, `broken`, `failed` or `unknown`, so a
script can branch on it.

The internal `run` mode is the exception in practice: it is the foreground host
of the instance, so it only ends when the supervisor stops the instance. It ends
with the same three codes.

## Diagnosing a broken registration

`status` reports the state of the registration:

| State | Meaning |
|---|---|
| `not-installed` | No registration exists for the scope. |
| `stopped` | Registered, but not running. |
| `starting` / `stopping` | The supervisor is transitioning. |
| `running` | The service is running. |
| `failed` | The service failed. |
| `broken` | Registered, but its executable is gone. |
| `unknown` | The supervisor did not report a usable state. |

A registration points at an **absolute executable path**. If the binary is
moved or deleted, the registration stays in place but cannot run: `status`
reports the registration as `broken` and names the missing executable, rather
than reporting a running service. The fix is to restore the binary at the
registered path, or to reinstall the service from the new path:

```bash
hfdesk service status             # shows the broken registration
hfdesk service install            # re-registers the current binary
```

## Per-platform notes

### Linux (systemd)

- The user scope is a **systemd user unit** (`~/.config/systemd/user/hfdesk.service`,
  or `$XDG_CONFIG_HOME/systemd/user/hfdesk.service`) and is driven with
  `systemctl --user`. The system scope is a machine-wide unit in
  `/etc/systemd/system`.
- **HFDesk never enables linger.** A user unit normally stops when the last
  session of the user ends. Booting without a login is an explicit decision of
  the user or the administrator (`loginctl enable-linger <user>`), and HFDesk
  does not make it for you.
- The unit keeps the process in the foreground (`Type=simple`) and restarts it
  on failure. It holds no access data.

### macOS (launchd)

- The user scope is a per-user **LaunchAgent** at
  `~/Library/LaunchAgents/com.hfdesk.hfdesk.plist`; the system scope is a
  **LaunchDaemon** at `/Library/LaunchDaemons/com.hfdesk.hfdesk.plist`.
- The registration is managed with `launchctl bootstrap`, `bootout` and
  `kickstart`. A job that is already loaded is re-bootstrapped so a changed
  property list takes effect.
- The property list holds the label, the program arguments and the environment
  of the instance — no credential.
- The **macOS CI job** is narrowed to the service package: the launchd
  manager and the property list rendering are darwin-only code, so the job
  runs `go test ./internal/service` (plus the build and the vet of the whole
  module) on a macOS runner. The full macOS suite has separate pre-existing
  macOS-compatibility gaps; the job stays narrowed until they are resolved.

#### Future app-bundle packaging

HFDesk ships a standalone CLI binary today. The LaunchAgent and the
LaunchDaemon are therefore registered by writing a property list into
`~/Library/LaunchAgents` or `/Library/LaunchDaemons`. That is the
**transitional path**.

On macOS 13 and later, Apple's
[`SMAppService`](https://developer.apple.com/documentation/servicemanagement/smappservice)
is the preferred API for registering a LaunchAgent or a LaunchDaemon that
**lives inside an application bundle**: the registration becomes part of the
bundle instead of a loose property list in a Library directory.

If HFDesk later gains a proper `.app`, the registration path must **migrate**
to `SMAppService`. The legacy property-list installation must not stay alive
inside the app-bundle flow, so the two mechanisms never register the same
service at the same time.

The migration is deliberately **not** implemented now: it would require a
Swift helper and an app bundle, and this work explicitly does not force
either.

**Consequence for the user:** a registration made by the current CLI keeps
working. A future app bundle takes the registration over through
`SMAppService`, and the app bundle and the CLI must not both register the
same service at the same time.

### Windows (Service Control Manager)

- The Service Control Manager is used **directly through the Go API**, never
  `sc.exe` and never PowerShell.
- Installing, removing, starting, stopping and restarting the service needs an
  **elevated shell**. HFDesk does not self-elevate and never invokes UAC.
- The service identity is `LocalService`, a least-privilege Windows service
  account, not `LocalSystem`.
- The service-owned state under `%ProgramData%\HFDesk` is ACL-restricted. The
  service does **not** recursively re-ACL or re-own an existing model or cache
  tree: changing the ownership of a large, possibly shared tree is an explicit
  decision of an administrator, not a side effect of an install.

### The identity of a machine-wide instance

A machine-wide (`--system`) instance runs as a **dedicated, stable and
unprivileged service account** of its platform. It never runs as root, it
never inherits the home directory of the user who runs the install command,
and it never inherits the personal profile of root.

| Platform | The instance runs as | The account |
|---|---|---|
| Linux (systemd, `--system`) | the account **`hfdesk`** | a dedicated system account; it must exist before the installation |
| macOS (launchd, `--system`) | the account **`_hfdesk`** | a dedicated system account (the underscore prefix is the macOS convention for a service account); it must exist before the installation |
| Windows (Service Control Manager) | **LocalService** (`NT AUTHORITY\LocalService`) | a documented minimum-privilege Windows service account. It is deliberately **not** `LocalSystem`: the instance needs no machine-wide privilege, and `LocalSystem` would give it more access than it uses. |

The account is **stable** on purpose: the files it owns have to stay writable
across reboots, so an ephemeral identity would break the instance at the next
restart. It is also **unprivileged**: the instance serves a dashboard and
downloads models, it does not administer the machine.

The name is the default of the platform and can be changed with
`--service-user ACCOUNT`. The option applies to the machine-wide scope only:
a user-scope instance runs as the user itself and never names an account.

**The account must already exist.** HFDesk is a service manager, not a system
provisioner: it never creates an account, never calls `useradd` or `dscl`,
never runs `sudo` and never elevates itself. An installation whose account
does not exist is **refused**, and the error names the account and the exact
commands that create it:

```bash
# Linux
sudo useradd --system --no-create-home --home-dir /var/lib/hfdesk --shell /usr/sbin/nologin hfdesk

# macOS
sudo dscl . -create /Users/_hfdesk UserShell /usr/bin/false
sudo dscl . -create /Users/_hfdesk UserHome /var/lib/hfdesk
sudo dscl . -create /Users/_hfdesk UniqueID <a free numeric id>
```

Creating the account needs **administrative rights**: the commands above are
run by an administrator, before the installation. HFDesk never creates an
account itself and never asks for a password.

**Only the three directories of the instance are granted to the account:** the
config directory, the state directory and the log directory — the directories
the installation creates anyway, together with the parent directories it
creates for them. A directory the installation **created** is granted to the
account; a directory that **existed before** the installation belongs to its
owner and is left alone, so the installation never takes a directory over from
the user who put it there. The account still has to be able to write into such
a directory: when it cannot, the installation is **refused** with the guidance
that names the directory, the account and the command that grants the access,
and the ownership of every directory stays as it was.

A model or cache tree is **never** chowned, never chmodded and never walked
recursively. A cache directory the user names explicitly is **validated** for
accessibility by the service account instead: when the account cannot reach
it, the installation fails with ownership/permission guidance, and the tree
stays exactly as it is.

**Consequences:**

- The cache of a machine-wide instance has to be **readable and writable by
  the service account**. Point the instance at a cache directory that the
  account can reach, or grant it access to that directory explicitly, or run
  a user-scope instance, which keeps the identity of the user and the access
  to their own Hugging Face cache.
- The Windows instance has a profile and a filesystem access of its own: it
  does not reuse `%APPDATA%` and it does not point at the model or cache
  directories of the interactive user.
- The **user-scope** instance is the one that keeps the identity of the user
  and the access to their own Hugging Face cache. It needs no service
  account.

### Privileged operations

Scope changes are operating-system operations performed by an administrator.
HFDesk never invokes `sudo`, `su`, `pkexec` or a UAC prompt. A `--system`
installation that lacks the rights fails with a permission error instead of
escalating on its own.

The **machine-wide scope** needs an **elevated shell** (root on Linux) for
`install`, `uninstall`, `start`, `stop` and `restart`: the registration lives
in a machine-wide location and the supervisor starts and stops the instance as
the system, not as the user who runs the command. A shell that is not elevated
fails with a permission error that names the requirement instead of escalating
on its own. The user scope needs no elevation: the instance belongs to the user
who runs the command.

## Upgrades

The registration points at an absolute executable path, so an upgrade is a
**binary replacement in place**:

1. Stop the service.
2. Replace the binary at the registered path.
3. Start the service again.

The registration does not need to be rewritten, because the path did not
change. If the binary *moves*, `status` reports the registration as `broken`
(see [Diagnosing a broken registration](#diagnosing-a-broken-registration))
and you reinstall the service from the new path.

The registered path is the **invoked** path, so a stable name keeps its name
in the registration: a symbolic link `/usr/local/bin/hfdesk` that points at
`/opt/hfdesk-1.2.3/hfdesk` is registered as `/usr/local/bin/hfdesk`, and the
next release replaces the file behind it. `install` warns when the registered
path is version-stamped (`/opt/hfdesk-1.2.3/hfdesk`), when the name of its
directory is version-stamped (`/opt/hfdesk-1.2.3/bin/hfdesk`) or when it
lives in a temporary directory, because such a path does not survive the next
upgrade.

When the binary that is run is not the one that should be registered, name it
explicitly:

```bash
hfdesk service install --executable /usr/local/bin/hfdesk
```

## See also

- [README](../README.md) — general project behavior
- [API](API.md) — HTTP endpoints, including the settings API used to set the
  token of a service instance
