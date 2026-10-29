# Quickstart: a Mac and a Windows machine in ten minutes

This is the path from nothing to two joined machines running a shared job: a Mac that hosts the hub (and is a node too) and a Windows PC that joins it. Nothing needs administrator rights except the one-time sandbox prerequisites below.

| Step | Where | Target time |
| --- | --- | --- |
| 1. Install and start the hub | Mac | 3 min |
| 2. Install and join | Windows | 3 min |
| 3. Run a shared job | Mac | 2 min |
| 4. Approve a grant in the UI | Mac | 1 min |
| 5. Next steps | | 1 min |

To try everything on one machine first, run `scripts/demo.sh` from a checkout: it starts a hub and a second node under temporary directories, posts to a channel, runs a job, approves a grant, prints the UI addresses and cleans up.

## Before you start (one time, not counted)

Jobs run only inside sandboxes, so the machine that runs a job needs its sandbox runtime:

- **Mac:** [Lima](https://lima-vm.io) 2.x: `brew install lima`. `mycel sandbox setup` (step 1) then fetches the VM image and boots a test VM once.
- **Windows:** WSL2: `wsl --install --no-distribution` in an administrator terminal, then reboot. That is the only manual step: `mycel sandbox setup` (step 2) creates the `mycel-sbx` sandbox distro. Jobs also need a Docker client (Docker Desktop) to fetch their images; setup warns when it is missing.

Both machines must be on the same network, and if the Mac's firewall is on, allow `mycel` to accept incoming connections when macOS asks.

## 1. The Mac: install and start the hub (3 min)

```sh
curl -fsSL https://raw.githubusercontent.com/Richard-Sen27/mycelium/main/scripts/install.sh | sh
```

This installs `mycel` to `~/.local/bin`. If the script says that directory is not on your PATH, add it as the script shows and open a new terminal. To build from source instead (needs Go 1.27): `sh install.sh --from-source`.

Start the hub. Other machines reach it at the Mac's mDNS name (`scutil --get LocalHostName` prints it) or at its LAN IP (`ipconfig getifaddr en0`):

```sh
mycel hub init --advertise https://<mac-name>.local:7777
mycel service install
mycel service status
mycel sandbox setup --yes
```

`mycel sandbox setup --yes` downloads the pinned Linux VM image for the sandboxes (a few hundred MB, once) and boots one test VM so the first job does not wait; `mycel sandbox status` shows the result. `hub init` prints the admin API key once and saves it in your config, so the CLI on the Mac is signed in as admin. `mycel service install` runs the daemon (hub and node) as a launchd agent: it starts now, at every login, and restarts if it stops. Its log is in `mycel service logs -f`. macOS shows a "Background Items Added" notification for it; that is expected (System Settings > General > Login Items lists it).

Create the Windows user's account and a join token for the PC:

```sh
mycel user add alice --role member          # prints alice's API key once: send it to her
mycel token create --name win --caps sandbox --ttl 15m
```

`token create` prints `mycel join <token>`: copy that line to the PC. The token works once, expires in 15 minutes, and carries the hub address and the hub CA fingerprint.

## 2. Windows: install and join (3 min)

In PowerShell (no administrator rights needed):

```powershell
irm https://raw.githubusercontent.com/Richard-Sen27/mycelium/main/scripts/install.ps1 -OutFile install.ps1
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

This installs `mycel.exe` to `%LOCALAPPDATA%\mycel\bin` and adds it to your user PATH; open a new terminal afterwards. Then join and keep the node running:

```powershell
mycel sandbox setup --yes
mycel join <token>
mycel service install
mycel service status
```

`mycel sandbox setup --yes` creates the `mycel-sbx` WSL2 distro that runs the sandboxes: it downloads a pinned Alpine Linux root filesystem (4 MB) and gVisor (about 100 MB), checks both against the checksums built into `mycel`, imports the distro, configures the distro so it doesn't mount your Windows drives and can't start Windows programs, installs the few tools the sandboxes need from Alpine's signed repositories, and runs a test command under gVisor. The isolation of jobs rests on gVisor, not on these distro settings. It takes a minute or two; run it again at any time to check and repair (`mycel sandbox status` shows the state). If it says WSL is missing, run `wsl --install --no-distribution` as administrator, reboot and run it again.

`mycel join` checks the hub's CA fingerprint against the token before trusting anything and then holds its own certificate. `mycel service install` registers a scheduled task that starts the daemon at your logon (in your session, so it can tell when you are using the PC) and restarts it on failure.

Sign the CLI in as alice so she can use the network from this PC too:

```powershell
mycel login https://<mac-name>.local:7777 --api-key <alice's key>
```

`mycel login` shows the hub's CA fingerprint and asks you to confirm it: compare it with the one `mycel hub init` printed on the Mac (or pass it with `--ca-fingerprint <hex>`).

## 3. Run a shared job (2 min)

Back on the Mac:

```sh
mycel nodes ls                       # both machines, online, with their inventory
mycel channels create '#team' --open
mycel job run --profile default --node win --channel '#team' -- sh -c 'echo hello from $(uname -a)'
mycel job logs <job id> --follow
mycel read '#team'                   # the job's result message
```

By default a node puts its owner first: remote sandboxes pause while someone is using the PC, so the job waits if you are at that keyboard. For this walkthrough, either step away from the PC or allow it to run anyway with `mycel nodes share win --local-first=false`.

The job runs in a fresh gVisor sandbox inside WSL2 on the PC: it cannot see the PC's files or processes, and reaches the network only through the egress allowlist. The other way round works the same: from the PC, `mycel job run --profile default --node <mac node name> -- …` runs in a Lima VM on the Mac.

## 4. Approve a grant in the UI (1 min)

Start a job that needs more than its profile allows, then ask for it:

```sh
mycel job run --profile default --node win --name fetch -- sh -c 'sleep 600'
mycel grants request --kind egress --allow pypi.org --subject job:<job id> --duration 1h --reason "install packages"
```

Open the UI with `mycel ui` on the Mac (it opens the hub at `https://localhost:7777`; the browser warns once because the hub's certificate comes from the hub's own CA), sign in with an API key, open Approvals and approve the request. `mycel grants ls` shows it active; it ends on its own after the hour, or at once with `mycel grants revoke <id>`. From the CLI the same is `mycel approve <id> --duration 1h`.

## 5. Where to go next (1 min)

- Wire a coding agent to the network: `mycel agent setup claude-code --agent coder` in a project directory (see the README, "Agents and jobs").
- Decide what each machine shares and when: `mycel node share --offer docker,ollama --when idle --local-first` on the machine itself, `mycel nodes share <name> …` as admin (README, "Sharing, grants and the kill switch").
- The kill switch: `mycel freeze --all --reason drill`, then `mycel unfreeze --all`.
- Service management: `mycel service status | stop | start | logs -f | uninstall` (uninstall keeps your data and logs).

## When the PC cannot reach the Mac

- Try the Mac's LAN IP instead of `<mac-name>.local` (Windows resolves `.local` names only on networks that pass mDNS). The address is fixed at `hub init`, so settle it before other machines join.
- `mycel hub discover` lists hubs announced on the local network, and `mycel join` falls back to them by CA fingerprint when the token's address does not answer.
- Venue Wi-Fi that blocks traffic between devices needs a relay on a machine with a public address: run `mycel relay serve` there, create the hub's relay URL with `mycel relay token create --name <hub> --host <relay host>`, and paste it into `mycel hub init --tunnel -` (or `mycel hub tunnel set -` for an existing hub; restart the daemon). `mycel hub tunnel status` shows the connection.
