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
- The supervisor starts the instance at boot of its scope when the registration
  is set to do so — a first installation writes that autostart, and a repeat
  installation keeps the autostart state it finds when its property list can
  be read (a property list that is missing or unreadable leaves no state to
  keep and gets the default that starts the instance; see
  [Upgrades](#upgrades)) — and restarts it when it fails.

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
  the machine-wide `start`, `stop` and `restart`. The `hfdesk service` command
  never elevates itself (see
  [Privileged operations](#privileged-operations)).
- The **service account** of a machine-wide installation on Linux and macOS,
  already created (see
  [The identity of a machine-wide instance](#the-identity-of-a-machine-wide-instance)).
  A user-scope installation needs no account: it runs as the user itself.
  Windows needs none either: the instance runs as the built-in `LocalService`
  account.
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
| `--service-user ACCOUNT` | Account a **machine-wide** instance runs as on Linux and macOS. Default: the service account of the platform (`hfdesk` on Linux, `_hfdesk` on macOS). Rejected for the user scope, and rejected on Windows, where the instance always runs as the built-in `LocalService` account and the identity cannot be chosen. |
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
It applies to the machine-wide scope on Linux and macOS: a user-scope instance
runs as the user itself, so the option is rejected there instead of being
ignored. Windows has no account to name: the instance always runs as the
built-in `LocalService` account, so the option is rejected there as well.

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
must be usable, and the runtime paths must be absolute. On Windows the
executable must also be a Windows executable: a truncated download and a file
that is not a program at all are refused before the running instance is
stopped. The machine type of the program is refused only when Windows reports
this host cannot execute it: a machine type the host supports as a guest is
accepted, and a support query Windows does not answer refuses nothing — the
installation continues instead of inferring an incompatibility it could not
establish. A failed installation restores the previous
registration instead of leaving a half-written one behind, except when the
rollback cannot prove that
the instance the failed update started has stopped: the failed registration is
kept then, so the registration and the process that runs never describe
different programs, and the error says so.

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

The instance uses the same logging pipeline as the interactive runs, so the
records of the server land in the log file of the instance as well. The
active file is rotated before a write would exceed 5 MiB, and three rotated
generations are kept, so the on-disk size of the log stays bounded.

The instance creates its log directory owner-only (`0700` on Unix) when it is
missing, and the log file keeps the owner-only mode (`0600` on Unix) of the
shared log file sink. It never changes the mode of a directory or of a file
that already exists: the log directory may be a directory that existed
before the installation and that belongs to someone else, and the instance
may only be able to write there. Tightening it needs an ownership the
installation never checked, and it would keep the instance from starting at
all.

A log file that cannot be written does not stop the instance: the instance
falls back to the console sink alone, reports the failure on the standard
error of the process and keeps serving. The failure of the log file never
changes the exit code of the host.

The records of the instance also reach the supervisor through the standard
error of the process: the systemd journal on Linux. On macOS the property
list sets **no** `StandardOutPath` and **no** `StandardErrorPath`: launchd
does not rotate the files it owns, so they would grow without bound. launchd
therefore discards them, and the bounded `hfdesk.log` inside the log
directory of the instance is the real log sink on macOS.

## Lifecycle commands

| Command | Effect | A service that is not installed |
|---|---|---|
| `install` | Registers the service with the supervisor and enables it for the boot of the scope on a **first** installation; a **repeat** installation keeps the autostart state the registration already has — on Linux it never runs `enable` at all, so an enabled systemd unit stays enabled, a unit enabled with `systemctl enable --runtime` stays runtime-only instead of being made permanent, and a disabled one stays disabled, and on macOS it never **leaves** an administrator's `launchctl disable` cleared: the two loads launchd requires run `enable` only for that load and restore the flag in the same operation — while a masked unit (persistent or `--runtime`) refuses the installation, and a Windows registration keeps its start type and its delayed-auto-start flag, a macOS registration keeps the `RunAtLoad` value of its property list, while a running Windows service with a disabled start type refuses the installation. A macOS first installation — no property list and no job launchd holds — registers the property list with `RunAtLoad`, so launchd starts the instance as soon as it is registered; a repeat installation — a stopped job launchd holds without its property list included — starts nothing that did not run, while a running job launchd holds without its property list refuses the installation before anything changes: no property list exists for an update to fall back on, and the refusal names the stop that clears the state. A systemd unit is only registered and enabled. The `State` line of the summary reports the state the supervisor reached. An existing registration is updated in place on every platform — the settings the command does not name again are kept, a running instance is restarted on the new program, and a stopped one stays stopped (see [Upgrades](#upgrades)). | It creates the registration. |
| `uninstall` | Removes the **registration only**. | Clean success: the registration is already gone. |
| `start` | Starts the registered service. | **Error** that says the service is not installed. |
| `stop` | Stops the registered service. | Clean success: there is nothing running to stop. |
| `restart` | Stops and starts the registered service. | **Error** that says the service is not installed. |
| `status` | Reports scope, state, registration path, executable, bind address, runtime paths and the effective cache root. | Reports the state `not-installed` and exits with `1`. |

The **machine-wide scope** needs an **elevated shell** for `start`, `stop` and
`restart`, exactly as it needs one for `install` and `uninstall` (see
[Privileged operations](#privileged-operations)). The user scope needs no
elevation: the instance belongs to the user who runs the command.

On Windows, `install`, `start` and `restart` also refuse — before the
service is touched — while a desktop instance of HFDesk serves its address:
the service would fight that desktop for the address. The refusal covers
the outcomes that start or restart the service: `start` and `restart`
always, `install` for a first registration (the registration it writes may
start the instance) and `install` over a running instance (which the
installation restarts); the elevated helper's update follows the same rule.
An `install` or update over an existing registration that is positively
confirmed stopped only rewrites the registration — it starts nothing — and
proceeds beside that desktop, while any state that is not positively
stopped (or cannot be read) fails closed with the same refusal. Every
refusal changes nothing and names the one step that ends it: quit HFDesk
from its notification-area icon and repeat the command. One further
refusal has no such step: a published claim that exists but cannot be read
or parsed leaves whether a desktop serves undetermined, so the command
refuses before it changes anything and states that cause alone — no single
step clears a file the guard cannot inspect.

The guard's reach is **per user and per session**: the singleton and the
published state of a desktop belong to one Windows session, so a desktop of
another user or session on the same address stays outside it. A start that
cannot claim its own instance degrades honestly instead of hiding: after this
release it never reports readiness from a foreign answer, so the collision
surfaces as a **failed start** — the instance never reports running, its bind
failure is written to the Windows Event Log of the service — the command
changes nothing, and the desktop that holds the address keeps serving.

The published state is a **claim of a live launch**: it carries the process
id of the desktop that wrote it, it is published **before** the launch opens
the window in which it binds the address — so a desktop that is about to
serve is already visible to the guard — and it counts only while that
process runs, so the file a crashed desktop left behind refuses nothing. An
elevation answered by another administrator carries the state path of the
**originating** desktop beside the singleton name, so the approving
account's own (always empty) claim directory can never hide a serving
desktop. A desktop host that cannot publish its claim stops serving instead,
so a serving desktop is never invisible to the guard. A stale claim whose
process id was reused by a live process reads as live; the resulting
refusal is transient and clears itself.

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

The summary reports the **effective registration**, not the request: an
installation over an existing registration names the settings that were kept,
so a preserved bind address or path is never displayed as the default of the
scope.

The commands are idempotent where the wanted state is already reached:

- `uninstall` on a service that is not installed succeeds; the registration is
  already gone.
- `start` on a service that already runs succeeds.
- `stop` on a service that is already stopped succeeds, and so does a `stop` on
  a service that is not installed at all: there is nothing running to stop.
- `install` over an existing registration updates it in place: the settings
  the command does not name again are kept, and an instance that is not
  running is not started.

`start` and `restart` are the two commands that need a registration to work on:
on a service that is not installed they fail with a readable "not installed"
error.

### Stopping an instance

A stop is a **graceful** shutdown, in two phases:

- The HTTP server is drained: it accepts no new request any more, and the
  requests that are already running finish — up to **10 s**.
- The job manager is closed: the running jobs are cancelled and drained, and
  the job list is written back to the state file — up to **10 s** again.

The orderly path therefore needs up to **20 s**.

The service layer publishes that budget and sizes every supervisor-side wait
after it, with a margin for the work the process still has to do around the
shutdown. The shared budget is **25 s**.

| Platform | Where the budget is published |
|---|---|
| Linux (systemd) | `TimeoutStopSec=25s` in the generated unit. |
| macOS (launchd) | `ExitTimeOut` 25 in the generated property list. |
| Windows (Service Control Manager) | the bounded wait of the service handler. |

launchd's own default of 20 s was exactly the total of the orderly path, so
the property list carries the budget of the service layer instead: the
deadline of a supervisor must never be shorter than the shutdown it waits
for.

The instance is **never killed to make a stop succeed**. The deadline of the
supervisor is the only hard stop, and it is larger than the orderly path
needs, so an instance that shuts down in time is never interrupted while it
saves its state.

A stop of an instance that is not running is a **clean success**: there is
nothing running to stop. A stop deletes nothing either: the stopped instance
keeps its job list and its download history, and the next start finds both
again.

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

The code follows the **state**, not the running fact: a registration that is
`broken` exits with `1` even when the instance it started still runs, because the
registration cannot start that instance again. Read the `State` and the `Detail`
of the output to tell a broken registration that still serves from one that does
not (see [Diagnosing a broken registration](#diagnosing-a-broken-registration)).

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
| `broken` | Registered, but its executable is gone. The instance the supervisor started can still be running; the `Detail` says so. |
| `unknown` | The supervisor did not report a usable state. |

A registration points at an **absolute executable path**. If the binary is
moved or deleted, the registration stays in place but cannot run: `status`
reports the registration as `broken` and names the missing executable, and the
detail says whether the instance still runs — a supervisor keeps the process it
started, so deleting the program stops the next start and not the running
instance. The fix is to restore the binary at the registered path, or to
reinstall the service from the new path:

```bash
hfdesk service status             # shows the broken registration
hfdesk service install            # re-registers the current binary
```

A reinstall repairs the registration and re-points it at the current binary and,
on Windows and Linux, keeps the state it finds: an instance that is not running
is not started, so the repaired registration stays stopped until
`hfdesk service start`, or until the service is started from the tray of the
Windows desktop app (see
[The Windows desktop host](#the-windows-desktop-host)). A running or starting
instance on those platforms is interrupted by the reinstall: it is stopped, and
only then is it started again on the current binary.

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

#### Unit ownership: the HFDesk unit and administrator drop-ins

Two parties touch the systemd registration, and each owns its own files:

- **HFDesk owns the unit file** — `hfdesk.service` in the unit directory of
  the scope (`~/.config/systemd/user/hfdesk.service`, or
  `$XDG_CONFIG_HOME/systemd/user/hfdesk.service`, for the user scope;
  `/etc/systemd/system/hfdesk.service` for the machine-wide scope). Every
  installation regenerates this file as a whole, byte deterministically, in
  exactly one shape: the canonical command `<program> service run --config …
  --state-dir … --log-dir … --listen …` with `--cache-dir`, `--hub-dir` and
  `--local-dir` behind it when the registration holds them, plus the fixed
  settings of the template (`Type=simple`, `Restart=on-failure`,
  `TimeoutStopSec=`, `Environment="HOME=…"` and the rest). The header carries
  a `# format: 1` marker that pins the rendering version. An update replaces
  the file with the rendering of the merged settings instead of editing it in
  place, so across an update only the location of the program and the options
  the command named again differ from the previous bytes: every unnamed
  setting keeps its registered value, and an optional directory that was
  never registered stays absent.
- **The administrator owns the drop-ins** — every `*.conf` file in
  `hfdesk.service.d/` beside the unit; `<unit directory>/hfdesk.service.d/local.conf`
  is the path the refusal messages recommend for customizations. HFDesk never
  writes, moves or deletes
  a drop-in, and it does not create the directory or the file — the
  administrator does, once. systemd composes the unit file and its drop-ins
  into the configuration it loads, and HFDesk judges its result by that
  loaded configuration. Every customization belongs there instead of into the
  unit file: environment values, environment files, supplementary groups, a
  working directory or account, restart policy, hardening, limits — except
  `ExecStart=`, which HFDesk owns: an `ExecStart=` override in a drop-in
  (`local.conf` included) is refused and must be **removed** from the drop-in
  that carries it, never moved into it, because the command of the instance
  comes only from the install flags. The ownership holds on a **first**
  installation as well: `install` scans the drop-in directory for
  `ExecStart=` before it writes anything and refuses over one, and after
  the reload it verifies that the supervisor loads the command it wrote.

**What an update preserves.** An installation over an existing registration
keeps everything the command does not name again:

- the settings of the registration — bind address, config/state/log paths,
  cache root, hub directory, output directory, service account — and the
  absence of an optional directory that was never registered;
- the running state: a running (or starting) instance is stopped, restarted
  on the new program and verified; a stopped, failed or never-started
  registration is never started by `install` — starting stays an explicit
  `service start`;
- the autostart state: `enable` runs **only at a first installation**. A
  repeat installation never runs `enable` at all, so the enablement the
  supervisor reports survives by non-interference: an enabled unit stays
  enabled, `enabled-runtime` stays runtime-only instead of becoming
  permanent, a disabled unit stays disabled;
- a masked unit — persistent or placed with `systemctl mask --runtime` —
  refuses the installation and is never unmasked: the error names the unmask
  command of the observed mask kind — `systemctl unmask hfdesk.service` for a
  persistent mask, `systemctl unmask --runtime hfdesk.service` for a runtime
  one — with the scope prefix of the command;
- the data of the instance: an update writes the unit file and, at most, the
  directories of the instance. It never touches the config file, the jobs,
  the history, the logs, the models or the cache, it never changes the
  ownership of a model or cache tree, and ownership is granted only to
  directories the installation created itself.

**The three refusals.** An update proceeds only over the registration HFDesk
owns. Three states refuse it **before anything changes** — nothing is
stopped, nothing is written, no directory is created — and each refusal says
first that the update was refused and the registration was not changed, then
names the cause, and then states exactly one instruction whose steps clear
that cause before the attempt runs again. The supervisor holds loaded unit
configuration in memory until a reload reads the file system again, so an
instruction that changes a file ends with `<scope> daemon-reload` and the
retry after it — a retry without that reload would read the same stale load
and refuse the same way.

1. **A hand-edited base file.** The bytes of `hfdesk.service` must be the
   canonical unit HFDesk generates. Any deviation refuses the update instead
   of being repaired or edited around: a changed, added or removed line or
   comment, a specifier or variable operand such as `%h` or `$NAME`, a second
   `ExecStart=`, a command that is the program alone or carries words the
   canonical vector does not — and an `env`-wrapped command such as `env -u
   NAME /path/to/hfdesk service run …` as well, which is refused as a hand
   edit of HFDesk's file rather than rewritten. The one instruction moves the
   customizations into the drop-in, removes the unit file, reloads the unit
   files and runs the installation again: the removal makes that attempt the
   first installation which writes HFDesk's canonical rendering (an edit the
   move alone cannot undo would otherwise refuse forever), and the reload
   clears the supervisor's copy of the removed file.
2. **A drop-in that owns the command.** `ExecStart=` is the field HFDesk
   owns. When systemd loads a command that differs from the one in the unit
   file — because a drop-in overrides it — the update refuses before any
   change and names the drop-in paths the supervisor reports. The one next
   action must clear that cause: it names the exact drop-in file that carries
   the override — *remove the `ExecStart=` override from that file* (with
   `local.conf` itself no exception) *and reload the unit files*, because the
   supervisor keeps the overridden command in memory until the reload reads
   the edited drop-in again — and when no reported drop-in carries one, it
   names the reload alone, which makes the supervisor read the command of the
   file again. The command of the instance is changed by naming the install
   flags again, never by a drop-in.
3. **A foreign or vendor fragment.** When systemd loads a unit under the
   HFDesk name from a file HFDesk did not write — a packaged or generated
   fragment — the update refuses instead of adopting it or writing over it.
   The one instruction depends on what systemd reports: a fragment file that
   is there is removed at that path after the customizations worth keeping
   moved into the drop-in, the unit files are reloaded and the installation
   runs again; a fragment file that is already gone (the load is stale until
   a reload) names only that reload and the retry; and a loaded registration
   that reports no readable fragment has no file to remove, so the refusal
   names no command and states that whatever loaded it must remove it before
   an installation can pass.

**What a drop-in may do.** Everything except the command. A drop-in that sets
`Environment=`, `EnvironmentFile=`, `SupplementaryGroups=`, `WorkingDirectory=`
or `User=` survives an update byte for byte — HFDesk never writes the file —
and systemd keeps following those values as the effective configuration. The
installation verifies after its write that exactly these effective values
came through unchanged unless the matching flag named a new one, and the
account and access checks of a later installation use the identity and the
working directory the supervisor really loads. Fields outside that
verification — restart policy, hardening, resource limits — are the
administrator's responsibility: HFDesk neither rewrites nor checks them, and
they stay in force as written.

**What an update proves — and what it promises when it fails.** Rewriting the
file is not success. After the write and the supervisor's reload, the
installation holds the loaded configuration against these proofs:

- the loaded command is exactly the canonical command that was written,
  compared as the typed argument vector the supervisor stores, not as a
  joined display string;
- every launch field HFDesk did not name — environment, environment files,
  supplementary groups, working directory, service identity — equals what
  the supervisor reported before the write;
- a restarted instance is proven to be the new one: the supervisor reports
  the unit running with the registered program, the argv of the live main
  process parses as the canonical `service run` vector, that very process
  answers the readiness probe at its effective listen address — the health
  document of HFDesk or its Basic-auth challenge, asked directly on the
  machine, without a credential and without following redirects — the
  instance reports the expected version when the build version is known, and
  the main process after the restart is a different process than the one
  snapshotted before it.

The Linux path proves the identity of the executed inode itself and survives
an image that was renamed away while another file took its old path.

A failure of any proof restores the previous registration: the saved unit
bytes are written back and read back, the supervisor reloads them, and the
loaded configuration is read again and compared with the pre-update snapshot.
Only when that verification passes — and only when the instance was running
before — is the previous instance started again. The failure report keeps the
original cause and states only what was proved: that the previous
registration and its loaded settings were restored, or that the restoration
could not be confirmed. An unconfirmed rollback is never reported as a
recovery.

**Never `StateDirectory=`.** HFDesk never writes `StateDirectory=`,
`LogsDirectory=` or `RuntimeDirectory=` into its unit: the directories of the
instance are created and granted explicitly by the installation, and a
systemd directory directive may recursively hand the ownership of an existing
tree to the supervisor. The rule extends to drop-ins as a contract warning:
such a directive in `hfdesk.service.d/*.conf` may re-own existing directory
trees behind the back of HFDesk, and the ownership of user data — models,
cache, jobs, logs — is never changed automatically. A drop-in that asks
systemd to manage those directories takes that risk outside what HFDesk can
preserve or verify.

**`status` describes, it does not refuse.** `hfdesk service status` reads
the registration as it finds it: a hand-edited unit is described as far as it
can be read — program, bind address, runtime paths, state — while the refusal
belongs to `install` and its update. The tolerant reading of `status` never
makes an update acceptable, and a refused update never changes what `status`
reports.

**Why the canonical form.** The canonical unit is deliberately the
verifiable domain: what HFDesk regenerates it can also prove after a restart,
and what it cannot prove — a command it can never read back from the live
process — it refuses up front instead of accepting an update it could never
confirm.

### macOS (launchd)

- The user scope is a per-user **LaunchAgent** at
  `~/Library/LaunchAgents/com.hfdesk.hfdesk.plist`; the system scope is a
  **LaunchDaemon** at `/Library/LaunchDaemons/com.hfdesk.hfdesk.plist`.
- The registration is managed with `launchctl bootstrap`, `bootout` and
  `kickstart`. A repeat installation updates the property list in place: the
  registered settings are merged with the request and the autostart key
  `RunAtLoad` keeps the value the property list carries. An instance that
  does not run is never loaded or started by the installation — the stale
  registration is unloaded when it was loaded, and a later start loads the
  property list the installation wrote — and an instance that runs is
  restarted on the new registration through the stop/start lifecycle of the
  manager.
- **HFDesk owns the property list it writes.** An update re-renders the file
  from the settings of the registration and preserves them through that
  merge: the registered program arguments (bind address, runtime paths and
  cache layout), the working directory, the service account of a
  machine-wide instance, and the `RunAtLoad` value. A key the HFDesk
  property list does not carry — a hand-added scheduling key such as
  `StartCalendarInterval` — does not survive an update, and a hand edit of
  the policy keys the property list does carry (`KeepAlive`, `ProcessType`,
  `ExitTimeOut`) is rewritten to HFDesk's rendering. A property list that
  cannot be read at all records no setting: the installation repairs it
  from the request instead of failing over it. The launchd override
  database is not part of the property list: a **first** installation
  clears the `launchctl disable` flag of the job it loads into the domain,
  and a repeat installation never **leaves** an administrator's
  `launchctl disable` cleared — launchd refuses to load a disabled label,
  so the two places that must load one (restarting a running disabled job
  and a rollback restoring a loaded disabled registration) run `enable`
  only for that load and restore the flag in the same operation —
  and clearing the flag starts nothing by itself. That database is read on
  every install, update and rollback, and the read fails closed: a
  `print-disabled` answer the supervisor cannot produce aborts the
  operation with that reason instead of being read as "not disabled" — an
  unknown enablement is never decided into an enable.
- The property list holds the label, the program arguments and the environment
  of the instance — no credential.
- The **macOS CI job** is narrowed to the service package and the command
  layer: the launchd manager and the property list rendering are
  darwin-only code, so the job runs `go test ./internal/service` and
  `go test ./cmd/hfdesk` (plus the build and the vet of the whole
  module) on a macOS runner. The repository has never run its full suite on
  macOS, so the job covers the darwin-only code and the platform-independent
  command layer, not the full suite; widening it to the full suite is a
  separate, deliberate step.

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
- Installing, removing, starting, stopping and restarting the service **from
  the command line** needs an **elevated shell**: the `hfdesk service` command
  does not self-elevate and never invokes UAC. The desktop host asks Windows for
  the rights through the standard prompt when the user chooses an install,
  update or start there (see
  [The Windows desktop host](#the-windows-desktop-host)); a declined prompt
  changes nothing.
- The installation sets the **failure recovery** for a new registration and
  for an existing registration only when a successful read proves its action
  list is empty: three automatic restarts after 5 s, 30 s and 60 s, with the
  failure count reset after one day without a failure. A nonempty action list
  and its existing reset period, non-crash flag, recovery command and reboot
  message are left unchanged; HFDesk does not add triggers to that policy. This
  includes an intentional `NoAction`, which can leave a failed service without
  an automatic restart. That is a limited exception for an existing custom
  policy, not the behavior for a new or proven-empty registration. For an empty
  list, setup preserves the recovery command and reboot message and enables
  non-crash recovery. An explicit user stop reports zero and is not a recovery
  signal. A newly enabled non-crash flag takes effect at the next start of
  Windows; a running machine applies it after its next boot. A failed read of
  the action list or a required policy field is not absence and causes no blind
  write. If writing the defaults fails, setup attempts to restore and read back
  the previous empty policy; an unverified restore is reported rather than
  claimed. Under the default schedule, the platform repeats the last restart
  (after 60 s) until the failure count resets, so a program that cannot come up
  is retried once a minute and never in a tight loop.
- The service identity is `LocalService`, a least-privilege Windows service
  account, not `LocalSystem`. It is **not configurable**: the built-in account
  has no name of its own and needs no credential, so a Windows instance
  cannot run as another account — HFDesk never registers one. An
  administrator who changes the account of the registration with the service
  control manager moves it outside what HFDesk updates keep: every update of
  that registration is refused until the supported identity is restored (see
  [Upgrading to a binary in another folder](#upgrading-to-a-binary-in-another-folder)).
- The service-owned state under `%ProgramData%\HFDesk` is ACL-restricted. The
  service does **not** recursively re-ACL or re-own an existing model or cache
  tree: changing the ownership of a large, possibly shared tree is an explicit
  decision of an administrator, not a side effect of an install.

### The Windows desktop host

The desktop host (`hfdesk-desktop*.exe`) runs the same server interactively and
is the only part of HFDesk that offers the service from a user interface. It is
a program of its own: a release ships it next to the console host, and neither
replaces the other.

- **Tray menu.** The menu has four actions: **Open HFDesk** (the web UI of the
  instance in the default browser), **Runtime / Logs** (the runtime window),
  **Install service…** (registers the Windows service) and **Quit HFDesk** (a
  graceful shutdown).
- **Install service…** asks for confirmation first, because the registration is
  a machine-wide change that outlives the app: the service starts at boot, keeps
  running without the desktop app, needs administrator rights, downloads and
  copies nothing, and keeps its own data. It then requires the console program
  in the folder of the desktop program — the program paired with this desktop
  program, the same name with the `hfdesk-desktop` prefix replaced by
  `hfdesk` — and installs the service from that folder: the desktop host never
  downloads and never copies a binary. A desktop program whose name carries a
  version has no other candidate: a missing pair is an error that names the
  console file it expected and the folder it looked in, so a new desktop
  program never registers the console program of another release and the
  version the report names is the version the registration runs. A desktop
  program without a version — a local build such as `hfdesk-desktop.exe` —
  keeps the fallbacks: `hfdesk.exe` next, then exactly one `hfdesk*.exe` of a
  release in the folder. Install the service from a folder that only
  administrators can modify: the service runs that program as `LocalService`,
  and HFDesk never changes the permissions of a user folder. Windows shows its
  standard administrator prompt, the elevated helper installs the service,
  starts it and verifies that it runs — that the server on the registered
  address answers as HFDesk, with the health document of the instance or, for a
  protected instance, its Basic-auth challenge — and reports the result in a
  message box. A desktop release also passes the version of that release as the
  expected version, and its report names a version only when the started
  instance reported it; a local desktop build names no release and claims none.
  PE candidates are checked for complete mandatory executable headers and
  sections without running them; Windows host and guest-machine support, not
  the helper's process architecture, decides known architecture incompatibility.
  If Windows cannot answer that support query, the candidate is not rejected on
  that basis alone. When updating a running service, access preflight uses its
  actual LocalService token and Windows `AccessCheck`; if that token cannot be
  obtained, access remains unknown rather than being inferred from a partial
  SDDL model.
  A declined prompt or a missing console program changes nothing and leaves the
  desktop app running.

  A first install starts and verifies the service through the same SCM handle
  that created its registration. A mismatching health version or failed
  readiness check stops and removes that registration only after the service
  process and unchanged command line are still proven to belong to this attempt;
  the service-owned data is never removed. If the process or registration has
  changed, or its state cannot be read, the helper does not reopen the name and
  risk stopping or deleting another installation: it keeps the registration and
  reports that cleanup could not be proven. A proven cleanup says that the
  desktop app can be started normally again. A registration already present at
  the first-install boundary is left untouched and reported as a conflict.
- **No second server.** Once the helper is started, the desktop app stops its
  own server through the graceful shutdown and ends. The helper starts the
  service only after that process has exited, so the desktop app and the service
  never bind the same address.
- **Persistence blocks a failed handover.** The desktop authorizes the service
  installation only for persistence that completed: a final history write that
  fails — or does not finish inside the handover budget — blocks the handover
  and with it the installation, by design. The strictness includes
  persistence: the jobs data itself is saved, yet a final write that cannot be
  confirmed still ends the attempt as a failure instead of authorizing the
  installation.
- **Separate data.** The service keeps its own data under `%ProgramData%\HFDesk`
  — its own config file, jobs, history and cache. The settings, jobs and models
  of the desktop app are **not** migrated and do not appear in the service.
- **Startup detection.** A launch that finds a registration — and no desktop
  app of its own already running — asks before anything serves: it starts no
  server of its own and publishes no state, except when a positively confirmed
  stopped service is answered with **Work in desktop mode**, the default
  button. That answer continues the launch exactly like a launch without a
  registration — and it is honored only after the service state is read again
  when the answer is taken: the dialog is modal, so a service that is no
  longer positively stopped at that re-read ends the launch with a notice
  instead of binding the address the service now needs. Every other answer
  ends it before anything serves. The gate never starts the service and never
  changes the registration.

  | Registered state | Buttons |
  |---|---|
  | running | **Open HFDesk** / **Update service** / **Cancel** |
  | broken, but the instance still runs | **Open HFDesk** / **Update service** / **Cancel** |
  | stopped | **Work in desktop mode** (default) / **Start service** / **Update service** / **Cancel** |
  | failed, or broken without a running instance | **Start service** / **Update service** / **Cancel** |

  A service that reports `starting` or `stopping` is not asked about: the
  launch waits for the state to settle and then asks about the state it
  reached — a service that came up offers **Open HFDesk**, a service that
  stopped offers **Work in desktop mode** as its default answer next to
  **Start service**. A service that is still starting or
  stopping when the wait ends is reported with a short notice that the launch
  changes nothing, and the launch starts no server of its own.

  The gate never treats `failed` or `broken` as a positively confirmed
  stopped service: those rows offer **Start service**, **Update service**
  and **Cancel** only (a broken registration whose instance still runs
  offers **Open HFDesk** instead of **Start service**), never **Work in
  desktop mode**. Returning to desktop mode after a failed first
  installation is covered by the normal cleanup path of that installation,
  which removes the registration it created when the machine had none
  before; when a registration remains after the attempt, remove it with
  `hfdesk service uninstall` from an elevated shell, and the next launch
  starts the desktop app normally.

  A registration that cannot be read is not a machine without one: an installed
  service may be running and bind the address this launch would bind. The
  launch starts no server, publishes no state and reports the failure with a
  notice that names `hfdesk service status` from an elevated shell and asks the
  user to start the desktop app again.

  **Work in desktop mode** continues the launch as a normal desktop app: it
  starts the server of this app, never starts the service and never changes the
  registration, so a stopped service stays stopped. **Open HFDesk** opens the
  registered address, **Start service** starts the
  registered service, and **Cancel** changes nothing. **Update service**
  re-registers the service from the folder of the new desktop program and keeps
  its settings and its running state: a running service keeps running on the new
  program, and a stopped service stays stopped. A registration that is broken or
  stopped is therefore re-pointed at the new program by an update, but it serves
  nothing until its program is restored and it is started explicitly (`Start
  service`, or `hfdesk service start`). A broken registration whose instance
  still runs is the exception: the update keeps that instance running on the new
  program, and the report names the address it serves.
- **The dialog** is a Windows task dialog, which needs the common controls
  version 6 that the embedded application manifest declares. A locally built
  binary without that resource still works: it falls back to message boxes that
  map Yes, No and Cancel onto the actions in their text. The stopped service
  needs two boxes for its four actions — the first picks the desktop mode, a
  service action or no action, and a stray Enter selects the desktop mode, the
  answer that changes nothing on the machine — while every other state keeps
  its single box.

### The identity of a machine-wide instance

A machine-wide (`--system`) instance runs as a **stable and unprivileged
service account** of its platform: a dedicated one on Linux and macOS, the
built-in `LocalService` account on Windows. It never runs as root, it never
inherits the home directory of the user who runs the install command, and it
never inherits the personal profile of root.

| Platform | The instance runs as | The account |
|---|---|---|
| Linux (systemd, `--system`) | the account **`hfdesk`** | a dedicated system account; it must exist before the installation |
| macOS (launchd, `--system`) | the account **`_hfdesk`** | a dedicated system account (the underscore prefix is the macOS convention for a service account); it must exist before the installation |
| Windows (Service Control Manager) | **LocalService** (`NT AUTHORITY\LocalService`) | a documented minimum-privilege Windows service account. It is deliberately **not** `LocalSystem`: the instance needs no machine-wide privilege, and `LocalSystem` would give it more access than it uses. The identity **cannot be chosen**: the built-in account has no name of its own and needs no credential, so it is the identity every Windows instance runs as. |

The account is **stable** on purpose: the files it owns have to stay writable
across reboots, so an ephemeral identity would break the instance at the next
restart. It is also **unprivileged**: the instance serves a dashboard and
downloads models, it does not administer the machine.

On Linux and macOS the name is the default of the platform and can be changed
with `--service-user ACCOUNT`. The option applies to the machine-wide scope
only: a user-scope instance runs as the user itself and never names an
account.

**Windows has no account to choose.** The instance runs as the built-in,
passwordless `LocalService` account, and `--service-user` is rejected there: a
service under a user account would need a credential, and a credential must
never reach the command line of an installation.

**The account must already exist.** That is a statement about Linux and
macOS: the Windows identity is an account of the operating system and is
therefore always there. HFDesk is a service manager, not a system
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
run by an administrator, before the installation. The `<a free numeric id>`
placeholder is not a runnable value: choose a numeric id that is not in use
and substitute it before running the third command. HFDesk never creates an
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

The **command line** never elevates: `hfdesk service …` invokes no `sudo`, `su`,
`pkexec` and no UAC prompt. A `--system` installation that lacks the rights
fails with a permission error instead of escalating on its own.

The **Windows desktop host** is the exception, and only on an explicit choice of
the user: **Install service…** in its tray, and **Update service** or
**Start service** in its startup dialog, ask Windows for administrator rights
through the standard prompt before an elevated helper performs the action (see
[The Windows desktop host](#the-windows-desktop-host)). A declined prompt
changes nothing: the tray installation leaves the running desktop app untouched,
while a declined **Start service** or **Update service** from the startup dialog
simply ends that launch, because it never started a server. Every
`hfdesk service` command keeps its guarantee: a machine-wide operation refuses
without an elevated shell and never prompts.

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
change. If the binary *moves*, `install` re-points the existing registration at
the new program instead (see below); `status` reports a registration whose
program is gone as `broken` (see
[Diagnosing a broken registration](#diagnosing-a-broken-registration)).

### Upgrading to a binary in another folder

On every platform `install` **updates an existing registration in place**
instead of replacing it, so an upgrade that installs the new release somewhere
else is one command. It is run from the new binary, or names that program
explicitly:

```bash
/opt/hfdesk-new/hfdesk service install
hfdesk service install --executable /opt/hfdesk-new/hfdesk
```

- **The settings are kept.** Every setting the command does not name again
  stays as the instance runs with it: the bind address, the config file, the
  state and log directory, the cache root, the hub directory, the local output
  directory and — on a machine-wide Linux or macOS instance — the service
  account. The program of the registration always comes from the invoked
  binary or `--executable`. A flag named again changes that setting, and
  `--cache-dir` without `--hub-dir` moves the hub directory with the cache
  root, exactly as it does on a first installation.
- **Native settings are kept too.** On Linux, an update regenerates the unit
  file HFDesk owns as its canonical rendering: the program always comes from
  the request, and a run option, the working directory or the service
  identity changes only when the matching install flag is named — every other
  registered value, including the absence of an optional directory, is
  rendered unchanged. Settings an administrator put in a drop-in are not part
  of that file at all: the drop-in survives the write and keeps being followed
  as the effective configuration (see
  [Unit ownership: the HFDesk unit and administrator drop-ins](#unit-ownership-the-hfdesk-unit-and-administrator-drop-ins)).
  An update proceeds only over the registration HFDesk owns: a base file that
  is not the canonical unit HFDesk generates — a hand edit, a specifier or
  variable operand, an `env`-wrapped command — a loaded command a drop-in
  owns, and a loaded fragment HFDesk did not write each refuse the update
  before anything changes, whether the instance runs or not, and each names
  its own single next action: the hand-edited file is reset by moving the
  customizations into the drop-in, removing the unit file and reloading the
  unit files; a drop-in-owned command is removed from the exact drop-in that
  carries it before that reload, while a loaded command no reported drop-in
  explains names the reload alone; a foreign fragment is removed at the path
  systemd reports before the reload — a fragment file that is already gone
  names only that reload — and a loaded registration with no readable
  fragment names no command at all. If systemd's loaded command
  differs from the unit file, the update is refused rather than editing an
  administrator-owned drop-in or claiming that a shadowed base-file edit took
  effect. On Windows, the
  readable SCM settings
  that HFDesk does not own are written back with the new command line. On
  macOS the property list HFDesk writes is regenerated from the merged
  registration the same way, and exactly what that preserves is stated under
  [macOS (launchd)](#macos-launchd). The
  Windows identity remains the fixed `LocalService` account, and an SCM password
  is never copied. An update never changes the account identity of a
  registration: one whose account was changed away from
  `NT AUTHORITY\LocalService` — with the service control manager, by an
  administrator — is refused before anything is written or stopped, and the
  error names the command that restores the supported identity, instead of
  the account being written back over the administrator's decision; a
  differently spelled name of that same account (a localized name or a SID
  string) is stored in its canonical spelling.
- Linux compares the loaded command through systemd's typed D-Bus argv
  properties. It does not infer argument boundaries from `systemctl show`'s
  joined display. If the loaded properties cannot be read, an existing update
  stops before changing the unit.
- **The autostart state is kept.** The start at boot is a setting of the
  registration and not of the command: only a **first** installation enables
  the registration it writes, and a repeat installation never undoes a
  decision of an administrator — on Linux it never runs `enable` at all, so
  a reinstall with a new binary changes nothing, and on macOS it never
  **leaves** an administrator's `launchctl disable` cleared (see the two
  loads launchd requires through `enable` below). The enablement the
  supervisor reports survives by
  non-interference: an enabled systemd unit stays enabled, a unit an
  administrator disabled stays disabled, and a unit enabled with `systemctl
  enable --runtime` keeps exactly that runtime-only enablement instead of
  being turned into a permanent one. On Windows the start
  type and the delayed-auto-start flag are written back as they are, so a manual
  or a disabled start stays manual or disabled and a delayed automatic start
  stays delayed. On macOS the property list keeps the `RunAtLoad` value it
  carries — a first installation (no property list and no job launchd holds)
  writes the default that starts the instance, a repeat installation with a
  readable property list keeps the found value, and a repeat installation
  over a property list that is missing or unreadable has no found value to
  keep and writes that default as well (the repair of the unreadable
  property list above) — and a repeat installation
  never leaves an administrator's `launchctl disable` cleared: the two loads
  launchd requires through `enable` — restarting a running disabled job and
  a rollback restoring a loaded disabled registration — restore the flag in
  the same operation, so it survives every update. Only a **first**
  installation — no property list and no job launchd holds — enables the
  registration it writes for the boot of its scope.
- **A state the installation cannot update is refused.** A masked systemd unit
  is refused before the unit file is written, whether the mask is persistent or
  was placed at runtime with `systemctl mask --runtime`: a persistent mask is a
  link to `/dev/null`, and writing the unit file would replace the mask, while a
  runtime mask lives in `/run` and leaves no file at the unit path, so the
  installation would register a unit that stays masked and cannot start. A
  runtime mask is visible only through the supervisor — when the supervisor
  cannot answer at all, the mask cannot be observed before the write; the
  attempt then fails at the reload with its honest report, and the mask is
  never touched, while a persistent mask is still caught before the write
  from its file. The
  error names the unmask command of the observed mask kind — `systemctl unmask
  hfdesk.service` for a persistent mask, `systemctl unmask --runtime
  hfdesk.service` for a runtime one — with the scope prefix of the
  command. A Windows service that is running while its start type is disabled is
  refused as well, before it is stopped: Windows will not start a disabled
  service again, so stopping it would leave the machine without the service that
  ran before. The error names the repair (`sc config hfdesk start= demand`, or an
  automatic start) and asks for another installation. A stopped Windows service
  with a disabled start type is allowed, because the installation does not start
  it. A Windows registration whose account was changed away from the supported
  `LocalService` identity is refused as well, before the registration is written
  and before the instance is stopped: an update keeps that identity and never
  writes the account back over the administrator's decision. The error names the
  repair (`sc config hfdesk obj= "NT AUTHORITY\LocalService" password= ""` from
  an elevated shell) and asks for another installation; a registration that runs
  as the supported identity keeps updating as before.
- **The program is checked before anything is touched — on Windows and
  Linux in full, on macOS within its documented scope and moment.**
  Windows and Linux check the path and effective execute access before they
  write a registration, create an instance directory or stop a running
  instance. Linux uses the
  kernel access check under the identity of the instance (including directory
  traversal and filesystem access rules); a native executable does not need a
  read bit. When the visible `binfmt_misc` state is readable and empty, a
  readable, short `#!` script is also checked for read access and an accessible
  interpreter. A visibly truncated ELF header, a missing interpreter named by
  an ELF `PT_INTERP` segment or shebang, and short plain text without a shebang
  are refused only under that same condition; if a handler is active or its
  state is unavailable, those formats remain unclassified.
  Linux does not reject an ELF solely because its machine differs from
  `runtime.GOARCH`, or reject another format merely because HFDesk cannot parse
  it: compatible architectures and registered handlers belong to the host.
  An unclassified format is not called verified. When an existing system-wide
  unit's context is preserved, preflight uses systemd's loaded `User`, `Group`
  and `SupplementaryGroups` for its access identity, and considers the loaded
  `WorkingDirectory` separately where needed (for example, to resolve a
  relative interpreter). If required context or effective access cannot be
  established, preflight logs that uncertainty; it is not treated as a denial
  or as verified access. For a running update, systemd's actual start/readiness
  check and the existing
  rollback remain authoritative for runtime behavior. On Windows, a running
  service update uses its actual `LocalService` process token for native
  `AccessCheck`, including its enabled traverse-bypass privilege. If that token
  cannot be obtained, access remains inconclusive rather than being inferred
  from a partial group model. macOS runs its own, lighter preflight — the
  program exists and carries an executable bit, the listen address parses and
  launchctl is available — after the account, directory and elevation checks
  and after the instance directories are prepared and granted, but still
  before the property list is written and before anything is loaded or
  stopped; it does not run the effective-access, image or machine checks of the
  other platforms. These executable access preflight checks do not
  change permissions.
- **The running state decides the restart.** An instance that runs, or is
  starting, is stopped and started again on the new program. The update requires
  the supervisor to report it running and the instance to pass readiness;
  program identity is checked separately as far as the platform can observe it.
  How that is proven differs by platform. On macOS the restart is the manager's
  own stop/start lifecycle: the graceful stop completes, launchd loads and
  starts the new property list, and the report follows what launchd accepted —
  without the readiness probe. On Windows the running process image
  is compared with the configured program, though Windows can refuse that read
  for a process of another account. In that case the update can still succeed
  only if readiness passes; it reports the running state and configured program
  path without claiming image identity. A known health-version mismatch fails
  independently. On Linux no process image
  is read: the installation asks systemd for the state and the `ExecStart` of
  the loaded unit, and it waits for the restarted instance to answer at its
  effective listen address with the same readiness probe the Windows handler
  uses. A static `--listen` value is taken from the unit; when its source is a
  dynamic systemd expression, HFDesk reads the service `MainPID` and that
  process's actual command line, then parses the service arguments to find the
  listen address. The source expression itself is not probed, and missing or
  unusable process context does not count as readiness. A unit that systemd
  reports active is a
  forked process and not a server — a process that dies on a bind error is
  reported active for a moment — so "runs" means the instance itself answers:
  the probe asks `/api/health` and accepts only a `200` that carries the health
  document of HFDesk (`status` `ok` and a version) or a `401` that carries the
  Basic-auth challenge of the instance (`Basic realm="HFDesk"`), and it sends
  no credential, so a foreign program that happens to listen on the port is not
  mistaken for the restarted instance. The probe does not follow redirects: a
  `3xx` answer is returned as it is and rejected, so a foreign server cannot
  point the probe at the health document of a real HFDesk on another address.
  Once resolved, the probe targets the instance's listen address directly on
  the same machine and never through a proxy configured in the environment, so
  an ambient proxy cannot fail a healthy instance. When the build version of
  the new program is known,
  the installation compares it too: an installation run from the registered
  binary knows its own build version, the desktop host passes the version of the
  release it installs, and an observed mismatch fails because the version of a
  running instance does not change. That mismatch remains a failure when Windows
  cannot read the process image. If both the image and a health version are
  unavailable, the update makes no version claim; an unknown check is not treated
  as a mismatching version, but this does not bypass readiness. One limit of
  that claim is acknowledged on Windows: the version claim opens the file at
  the executable's path, so an image that was renamed away while another file
  took its old path lies outside what it can see, and an identity the
  platform cannot prove withholds the claim instead of guessing. A
  Basic-auth challenge proves readiness without a version, and a local
  `dev` build asks readiness without a release expectation.
  A first desktop install must pass the service-owned readiness check before it
  reports success. An instance that was stopped, failed, or absent is **not**
  started by the CLI: `hfdesk service install` never starts a service that was
  not running, and starting stays an explicit `service start`. A registration
  that is `broken` follows the
  running fact and not the state: an instance the supervisor still runs is
  restarted on the new program like any other running one, and a broken
  registration whose instance is gone is not started.
- **A failure says what it restored.** A failure before the registration is
  written leaves a running instance untouched. A failure after the registration
  was written restores the previous registration: its configuration is written
  back, and the failure recovery the attempt configured is not part of that
  restore and stays in place. A configuration write the platform refuses part way
  through is read back and the previous configuration is restored before the
  failure is reported — Windows writes the program, the account, the delayed
  start and the description in separate calls, so the command line can already be
  replaced when a later one fails — and the report says which of the two it
  proved: that the previous configuration is in place again, or that the
  registration may be partially changed together with the reason. The running
  instance is not stopped or started on that path. On Linux, a restart rollback
  starts the previous program only after the previous unit file is read back, a
  daemon reload succeeds and systemd reports the previous command, environment,
  environment files and launch identity again. If any of those checks fails, no
  recovery restart is issued and the error retains the installation failure
  while describing the unconfirmed rollback. On Windows, the restart rollback
  stops the instance the failed update started before it restores the previous
  configuration, and when that stop cannot be proven it keeps the failed
  registration instead of leaving a registration that names another program
  than the one that runs. On Windows, installation-created config, state and
  log directories are removed only after a no-write outcome or a rollback that
  verifies restoration of the previous registration or removal of the new one;
  otherwise they are retained. Cleanup is best effort and removes only empty
  directories. The error names the proven outcome in every case, so a failed
  installation never reports success from the registration write alone.

macOS is updated in place as well now: `install` there merges the request
with the property list on disk, keeps the settings the command does not name
again and the `RunAtLoad` value, never starts an instance that was stopped and
restarts a running one on the new program through the manager's own stop/start
lifecycle — with the existing verification depth of macOS, which reports the
registration launchd accepted and does not run the readiness probe of the
platforms above. The stop/replace/start procedure above therefore stays the
upgrade path that needs no installation.

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
