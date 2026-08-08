<div align="center">

<h1>
  <img src="assets/vmxplore-avatar.svg" width="84" align="middle" alt=""/>
  &nbsp;vmxplore
</h1>

**The KVM console your distro never shipped — the whole estate, one window.**

*Every domain joined to the storage under it: lineage, snapshots, consoles, clones.*

[![License: BSD-3](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)
![Platform](https://img.shields.io/badge/platform-Linux-brightgreen.svg)
![Built with Go](https://img.shields.io/badge/built%20with-Go-00ADD8.svg)
![KVM](https://img.shields.io/badge/hypervisor-KVM%2Flibvirt-red.svg)
![Wayland](https://img.shields.io/badge/native-Wayland%20%7C%20X11-7a2fe0.svg)

**The family:** [kldload](https://github.com/kldload/kldload) — the substrate &middot; [zxplore](https://github.com/zxplore/zxplore) — the ZFS console &middot; [wgxplore](https://github.com/wgxplore/wgxplore) — the WireGuard console &middot; **vmxplore** — the VM console

<img src="assets/screenshots/estate-annotated.png" width="960" alt="vmxplore annotated — the estate tree on the left, a live console middle-right, the full domain dossier and verb toolbar below"/>

<sub><em>One window: the estate tree, a live console, the full dossier, and every verb — annotated.</em></sub>

</div>

KVM is already in your kernel. libvirt already manages it. What stock Linux
never shipped is the *console*: one window where the estate lives. vmxplore is
that window — a native GUI (and a static TUI for ssh) over the libvirt you
already have. Nothing replaced, no appliance, no agent; `virsh` is never
fought, only shown.

## Highlights

- **A modern VNC client, built from scratch** — the graphics console is a
  hand-rolled RFB implementation rendering the guest's framebuffer natively
  into the window, with full mouse, keyboard, and two-way clipboard. **No
  websockify, no noVNC, no virt-viewer** — no bridge process at all. It talks
  straight to qemu's VNC server (over loopback locally, over ssh for a remote
  host), and the GUI runs natively on **Wayland** (and X11).
- **An in-app serial console** — a real terminal on `virsh console`, in the
  same window: boot messages, login prompts, headless ttys, copy-paste and all.
- **Remote management over `qemu+ssh`** — `--connect` a headless hypervisor and
  the entire tool follows: estate, verbs, the ZFS join, *and* both consoles,
  authenticated with the same ssh key as your shell. No agent on the far side.
- **Golden → clone → fleet, on ZFS** — seal a VM into a golden and stamp out N
  zero-copy clones in one gesture; blocks are shared until a clone diverges.
- **New VM your way** — a cloud image (cloud-init) *or* boot the distro's own
  installer ISO and do it by hand; either way with a custom first-boot script.
- **Batch operations** — check many VMs and Start / Stop / Reboot / Delete them
  all at once.
- **One static binary, capability-tiered** — copy `vmx` to any libvirt box; the
  extra powers light up by probe (ZFS, then kldload), never a licence check.

**What you're looking at** — one screen, no hidden state:

- **The estate tree, left** — every domain, grouped (off groups collapse
  themselves), each row two lines deep: name · state · CPU over specs · guest
  IP · zvol usage · snapshot count · a `⑂` when it's a clone. Colour reads at
  a glance — green running, brown dormant, amber flagged. Dot-click to batch-
  select; ctrl/shift-click for a range; then Start / Stop / Reboot / Delete the
  whole set at once.
- **The console, right-top** — three tabs (below).
- **Details & actions, right-bottom** — the full dossier (uuid, disks, IPs,
  dataset, clone lineage, snapshot classes) and the verb toolbar. Every
  mutation shows its exact `virsh`/`zfs` command and is audit-logged; the
  destructive ones arm only when you retype the domain name.

## The three console tabs

The console pane is the heart of it — pick a running VM and the right tab
attaches automatically, in-window, no external viewer.

### Serial

A real terminal on `virsh console`, right in the pane — boot messages, a login
prompt, a headless server's tty. Copy-paste, resize, the works.

### Graphics — a native VNC client, no bridge

<img src="assets/screenshots/console-vnc.png" width="820" alt="the Graphics tab rendering a guest's framebuffer natively"/>

<sub><em>The Graphics tab: a guest's boot console rendered by a hand-written VNC client — no bridge, no external viewer.</em></sub>

A from-scratch RFB (VNC) client renders the guest's framebuffer directly into
the window, with full mouse, keyboard, and two-way clipboard. It speaks to
qemu's VNC server over loopback (or over ssh for a remote host) — **no
websockify, no noVNC, no virt-viewer.** The picture the web tools bridge to,
delivered natively.

### kldload — the substrate's toolset, one click

<img src="assets/screenshots/kldload-tools.png" width="900" alt="the kldload tab: a tile launcher for cluster builds, golden images, exports, and demos"/>

<sub><em>The kldload tab on a kernel-loaded substrate: the whole toolset — clusters, goldens, exports, demos — one tile away, running right in the pane.</em></sub>

On a plain libvirt host this tab pitches the OS. On a **kldload** host it
becomes a command center — a tile launcher for the whole kernel-loaded
substrate: instant ZFS-clone clusters (`kspawn`), Kubernetes-on-ZFS
(`kube-cluster`), golden cloud-init images (`kimage`), Windows goldens
(`kvm-win`), nine-format VM exports (`kexport`), guided demos, and more, each
running right in the tab. Same one binary — the abilities light up by
capability probe, never a licence check.

## Build a VM the way you want

<table>
<tr>
<td width="50%" valign="top">
<img src="assets/screenshots/new-vm.png" alt="New VM dialog — cloud image or installer ISO, with a custom post-install script"/>
<br/><sub><em>New VM — cloud image or installer ISO, with a first-boot script.</em></sub>
</td>
<td width="50%" valign="top">
<img src="assets/screenshots/ez-fleet.png" alt="EZ Fleet dialog — build a golden and N clones in one shot"/>
<br/><sub><em>EZ Fleet — one golden, N zero-copy clones, one click.</em></sub>
</td>
</tr>
<tr>
<td valign="top">

**New VM** — a cloud image (cloud-init configures it, boots ready-to-ssh) *or*
an installer ISO (boot the distro's own installer and run apt/dnf/pacman the
normal way — any ISO, incl. an Arch live ISO or a RHEL DVD). Paste a
**post-install script** and it runs as root on first boot: build your own
appliance, then seal it into a golden.

</td>
<td valign="top">

**EZ Fleet** — pick a distro and a number: it builds one golden, seals it, and
stamps out N zero-copy ZFS clones in a single gesture. "Give me five Fedora
boxes," done. On a ZFS host cloning is instant and near-free — blocks are
shared until a clone diverges.

</td>
</tr>
</table>

## The capability ladder

vmxplore is tiered by what the host *can do* — probes, never licence checks:

| Host | What lights up |
|---|---|
| any libvirt box | the full console: estate, serial + VNC consoles, lifecycle verbs, New VM |
| + OpenZFS | the storage join: clone lineage, snapshot classes, rollback, instant clones, golden → fleet |
| + [kldload](https://kldload.com) | the tool launcher: cluster builds, golden images, Windows unattended goldens, nine-format exports, guided demos |

The third tier is the interesting one: those tiles aren't features of vmxplore
— they're live demonstrations of what a **kernel-loaded substrate** looks like
when ZFS, KVM, WireGuard and the tooling are assembled with opinions. vmxplore
just gives it a stage. The generic tool recruits the user; the substrate is
what the console makes effortless.

## Install

Any Linux with KVM/libvirt (be in the `libvirt` group). Build deps for the GUI
are cgo + OpenGL headers — the Makefile header carries the exact per-distro
package list. Headless boxes can skip them entirely and build just the TUI.

```
git clone https://github.com/vmxplore/vmxplore && cd vmxplore
make            # vmxplore (GUI+TUI) + vmx (static TUI-only)
sudo make install
```

```
vmxplore                 # the GUI
vmx --tui                # the terminal UI (any ssh session)
vmx --once               # print the estate table and exit (scripts)
vmx --connect HOST       # drive a remote hypervisor over qemu+ssh
```

## Remote

`--connect <host | user@host | qemu+ssh://host/system>` points the whole tool
at a headless hypervisor over ssh — the estate, the verbs, the ZFS join, the
serial and VNC consoles all follow. Same key and `known_hosts` as your shell;
nothing new to configure. (In the GUI, the **Connect** button on the estate
header does the same.)

## Grouping rules

Estate grouping and snapshot classification are data, not code: rules files
(embedded generic + kldload profiles, `/etc/vmxplore/rules` or `--rules` to
override). The core contains no site-specific strings.

## License

BSD-3-Clause. Part of the kldload family; built for [vmxplore.dev](https://vmxplore.dev).
