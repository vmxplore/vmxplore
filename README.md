<div align="center">

<h1>
  <img src="packaging/vmxplore.svg" width="76" align="middle" alt=""/>
  &nbsp;vmxplore
</h1>

**The KVM console your distro never shipped — the whole estate, one window.**

*Every domain joined to the storage under it: lineage, snapshots, consoles, clones.*

[![License: BSD-3](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)
![Platform](https://img.shields.io/badge/platform-Linux-brightgreen.svg)
![Built with Go](https://img.shields.io/badge/built%20with-Go-00ADD8.svg)
![KVM](https://img.shields.io/badge/hypervisor-KVM%2Flibvirt-red.svg)

**The family:** [kldload](https://github.com/kldload/kldload) — the substrate &middot; [zxplore](https://github.com/zxplore/zxplore) — the ZFS console &middot; [wgxplore](https://github.com/wgxplore/wgxplore) — the WireGuard console &middot; **vmxplore** — the VM console

</div>

KVM is already in your kernel. libvirt already manages it. What stock Linux
never shipped is the *console*: one window where the estate lives. vmxplore
is that window — a native GUI (and a static TUI for ssh) over the libvirt
you already have. Nothing replaced, no appliance, no agent; `virsh` is
never fought, only shown.

- **The estate list** — every domain, live state, CPU, grouped and filtered.
- **Real consoles, in-app** — a serial terminal *and* a from-scratch native
  VNC client, both attaching automatically to whatever you select. No
  websockify, no noVNC bridge, no external viewer.
- **Safe verbs** — start, shutdown, force-off, snapshot, rollback, clone,
  vCPU/memory, autostart. Every mutation shows the exact commands it will
  run and asks first; destructive verbs arm only when you retype the
  domain name; every run is appended to an audit log.
- **The ZFS join** — on a ZFS host, each VM row knows the zvol under it:
  clone ancestry, classified snapshots, blocks shared until divergence.
  Cloning a VM is a zero-copy `zfs clone` + `virt-clone`, in seconds.

## Install

Any Linux with KVM/libvirt (be in the `libvirt` group). Build deps for the
GUI are cgo + OpenGL headers — the Makefile header carries the exact
per-distro package list. Headless boxes can skip them entirely:

```
git clone https://github.com/vmxplore/vmxplore && cd vmxplore
make            # vmxplore (GUI+TUI) + vmx (static TUI-only)
sudo make install
```

```
vmxplore        # the GUI
vmx --tui       # the terminal UI (any ssh session)
vmx --once      # print the estate table and exit (scripts)
```

## The capability ladder

vmxplore is tiered by what the host *can do* — probes, never license checks:

| Host | What lights up |
|---|---|
| any libvirt box | the full console: estate, consoles, lifecycle verbs |
| + OpenZFS | the storage join: lineage, snapshot classes, rollback, instant clones |
| + [kldload](https://kldload.com) | the tool launcher: one-click cluster builds, golden images, Windows unattended goldens, nine-format VM exports, guided demos |

The third tier is the interesting one: those tiles aren't features of
vmxplore — they're live demonstrations of what a substrate looks like when
ZFS, KVM and the tooling are assembled with opinions. vmxplore just gives
it a stage.

## Grouping rules

Estate grouping and snapshot classification are data, not code: rules files
(embedded generic + kldload profiles, `/etc/vmxplore/rules` or `--rules` to
override). The core contains no site-specific strings.

## License

BSD-3-Clause. Part of the kldload family; built for [vmxplore.dev](https://vmxplore.dev).
