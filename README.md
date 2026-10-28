# Guest agent for Tart VMs

A guest agent for Tart VMS is a lightweight background service that runs inside the virtual machine and enables enhanced communication between the host and guest and other useful features, such as automatic disk resizing.

Currently implemented features:

* Automatic disk resizing for macOS VMs with recovery partition removed (`--resize-disk`)
    * needs to be invoked as a launchd [global daemon](https://launchd.info/)
* Multi-format clipboard sharing for macOS and Linux (Wayland / X11 / XWayland) VMs using SPICE vdagent implementation (`--run-vdagent`)
    * supports UTF-8 text and images (PNG, TIFF, JPG, BMP) with automatic PNG compression and transcoding
    * on macOS: invoke as a launchd global agent
    * on Linux: invoke as a systemd user service with `WAYLAND_DISPLAY` or `DISPLAY` and `XDG_RUNTIME_DIR` (uses `golang.design/x/clipboard` pure-Go Wayland and X11 socket backends; no Cgo or external C libraries required). Ensure user has read/write permission to `/dev/virtio-ports/com.redhat.spice.0` or `/dev/vport*` via device group membership (e.g. `dialout`) or a udev rule (`SUBSYSTEM=="virtio-ports", KERNEL=="vport*", MODE="0666"`)
* SPICE File Transfer Daemon (`VD_AGENT_FILE_XFER_*`)
    * automatically receives files transferred over SPICE into `~/Downloads` with collision deduplication and path traversal sanitization
* `tart exec` support (`--run-rpc`)
    * it's recommended to invoke it as a launchd [global agent](https://launchd.info/) because fewer privileges will be available to commands started via `tart exec`
    * however, you can also invoke it as a launchd [global daemon](https://launchd.info/) or systemd system service if running commands started via `tart exec` as `root` is desired
* `tart ip --resolver=agent` support (`--run-rpc`)
    * allows resolving VM's IP address without relying on DHCP leases and/or an ARP table
* Environment and capability diagnostics (`tart-guest-agent doctor` or `--doctor`)
    * checks SPICE serial port accessibility, Wayland/XWayland display sessions, clipboard backends, system CLI tools, and file transfer download targets with actionable remediation hints
* System Tray / Status Bar and Notifications (`tart-guest-agent tray` or `--run-tray`)
    * visual status indicator for active guest services with quick menu actions
    * interactive notifications panel (`tart-guest-agent notifications` or `--notifications`) showing recent clipboard and file transfer events
    * graphical settings and preferences panel (`tart-guest-agent settings` or `--settings`) to toggle notifications, image clipboard, file transfer, and download folders
    * desktop notifications for agent startup and completed file transfers

To run all features appropriate for a given context, use component groups:

* `--run-daemon`
    * implies `--resize-disk` 
    * example usage: [`tart-guest-daemon.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-daemon.plist)
* `--run-agent`
    * implies `--run-vdagent --run-rpc` 
    * example usage: [`tart-guest-agent.plist`](https://github.com/cirruslabs/macos-image-templates/blob/main/data/tart-guest-agent.plist)

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
