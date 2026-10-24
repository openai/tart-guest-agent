# Guest agent for Tart VMs

A guest agent for Tart VMS is a lightweight background service that runs inside the virtual machine and enables enhanced communication between the host and guest and other useful features, such as automatic disk resizing.

Currently implemented features:

* Automatic disk resizing for macOS VMs with recovery partition removed (`--resize-disk`)
    * needs to be invoked as a launchd [global daemon](https://launchd.info/)
* Clipboard sharing for macOS VMs using our in-house SPICE vdagent implementation (`--run-vdagent`)
    * needs to be invoked as a launchd [global agent](https://launchd.info/)
* `tart exec` support (`--run-rpc`)
    * it's recommended to invoke it as a launchd [global agent](https://launchd.info/) because fewer privileges will be available to commands started via `tart exec`
    * however, you can also invoke it as a launchd [global daemon](https://launchd.info/) if running commands started via `tart exec` as `root` is desired
* `tart ip --resolver=agent` support (`--run-rpc`)
    * allows resolving VM's IP address without relying on DHCP leases and/or an ARP table

To run all features appropriate for a given context, use component groups:

* `--run-daemon`
    * implies `--resize-disk` 
    * example usage: [`tart-guest-daemon.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-daemon.plist)
* `--run-agent`
    * implies `--run-vdagent --run-rpc` 
    * example usage: [`tart-guest-agent.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-agent.plist)

## Installation

### Linux

Prebuilt packages are available on the [Releases](https://github.com/openai/tart-guest-agent/releases) page for `.deb`, `.rpm`, and `.apk` distributions.

**Debian / Ubuntu:**
```sh
sudo dpkg -i tart-guest-agent_*_linux_arm64.deb # or linux_amd64.deb
sudo systemctl daemon-reload
sudo systemctl enable --now tart-guest-agent.service
```

**Fedora / RHEL:**
```sh
sudo rpm -i tart-guest-agent_*_linux_arm64.rpm # or linux_amd64.rpm
sudo systemctl daemon-reload
sudo systemctl enable --now tart-guest-agent.service
```

**Alpine:**
```sh
sudo apk add --allow-untrusted tart-guest-agent_*_linux_arm64.apk # or linux_amd64.apk
```

### macOS

#### Via Homebrew

```sh
brew install openai/tools/tart-guest-agent
```

#### Manual Installation

1. Download the latest universal archive (`tart-guest-agent-darwin-all.tar.gz`) from [Releases](https://github.com/openai/tart-guest-agent/releases) and place the executable at `/usr/local/bin/tart-guest-agent`.
2. Configure launchd services:
   - For automatic disk resizing: copy [`tart-guest-daemon.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-daemon.plist) to `/Library/LaunchDaemons/com.cirruslabs.tart-guest-daemon.plist`.
   - For clipboard sharing and `tart exec`: copy [`tart-guest-agent.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-agent.plist) to `/Library/LaunchAgents/com.cirruslabs.tart-guest-agent.plist`.
3. Load the services:
   ```sh
   # Load daemon
   sudo launchctl bootstrap system /Library/LaunchDaemons/com.cirruslabs.tart-guest-daemon.plist

   # Load agent (run as the target GUI user)
   launchctl bootstrap gui/$(id -u) /Library/LaunchAgents/com.cirruslabs.tart-guest-agent.plist
   ```

## Wrapping RPC commands

An image administrator can configure a fixed command prefix with repeated
`--exec-wrapper` flags. The guest agent appends the requested executable and its
arguments without shell interpolation. The prefix applies to every Exec RPC,
including interactive, PTY, detached, and user-override commands; clients cannot
disable it. With no prefix, execution is unchanged.

For example, a managed image can add an environment variable to every command:

```sh
tart-guest-agent --run-agent \
  --exec-wrapper=/usr/bin/env \
  --exec-wrapper=-- \
  --exec-wrapper=MANAGED_IMAGE=example
```

The first argument must be an absolute path to a regular file that the guest
agent's effective user can execute. Invalid configuration stops startup. A wrapper
that cannot start never falls back to running the requested command directly.
Attached commands return the wrapper's
exit status; detached commands retain their existing process-start acknowledgment.
Use a wrapper that replaces itself with the command so signals and exit handling
retain their usual behavior.

The wrapper receives the command name unchanged and handles its executable
lookup. Include its end-of-options marker in the prefix when its interface
requires one.

The wrapper runs with the command's requested environment, working directory,
and user. A requested user must also be able to execute the wrapper. Keep its
executable, configuration, and launch settings under the image administrator's
control, and choose a wrapper whose behavior remains correct
under those overrides. Only commands started through the guest agent use this
prefix.
