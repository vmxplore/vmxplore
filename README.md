<div align="center">

<h1>
  <img src="assets/vmxplore-avatar.svg" width="84" align="middle" alt=""/>
  &nbsp;vmxplore
</h1>

**Turn any Linux distro into a powerful hypervisor — GUI *and* TUI, one binary.**

*The KVM console your distro never shipped: the whole estate, one window.*

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
never shipped is the *console* — one window where the estate lives. vmxplore is
that window: a native GUI (and a static TUI for ssh) over the libvirt you
already have. Nothing replaced, no appliance, no agent, no reinstall; `virsh`
is never fought, only shown.

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
- **Appliances — a self-hosted app as one button** — pick an app, fill in its
  two or three fields, and get a configured VM running it. Every "how to
  self-host X" writeup is the same four moves (fetch a pinned artifact, write a
  config, init a database, drop a unit file); the catalog encodes that once per
  app, so a weekend of following a blog post becomes a click.
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
delivered natively, on Wayland.

### kldload — the substrate's toolset, one click

<img src="assets/screenshots/kldload-tools.png" width="900" alt="the kldload tab: a tile launcher for cluster builds, golden images, exports, and demos"/>

<sub><em>The kldload tab on a kernel-loaded substrate: the whole toolset — clusters, goldens, exports, demos — one tile away, running right in the pane.</em></sub>

On a plain libvirt host this tab pitches the OS. On a **kldload** host it
becomes a command center — a tile launcher for the whole kernel-loaded
substrate (see [Enhanced on kldload](#enhanced-on-kldload)). Same one binary —
the abilities light up by capability probe, never a licence check.

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

## Appliances — an app, running, in one gesture

<img src="assets/screenshots/appliance-writefreely.png" alt="WriteFreely Desktop appliance — the VM boots straight into the editor, signed in" width="100%"/>
<sub><em>WriteFreely Desktop: power on, write. No login prompt, no desktop to
navigate — the graphics console <strong>is</strong> the application.</em></sub>

Pick an entry, answer its handful of app-specific questions, and the ordinary
New VM pipeline builds it: cloud image, cloud-init, a fixed post-install script,
then a wait until the app actually answers on its port — at which point you are
handed its real URL.

The catalog is **data, not code**. An entry is a struct literal plus a bash
string, so adding an app never touches the pipeline, the GUI or the tests. Two
consequences worth knowing:

- **The generated script is a useful artifact on its own.** `--appliance-script`
  prints it without building anything: ordinary bash with no vmxplore, libvirt
  or ZFS dependency in it, which an upstream project could publish as their own
  "install on a fresh VM" page. It also means you can *read exactly what the
  button is about to run*.
- **Operator input is never interpolated into a script body.** The body is fixed
  bash reading named variables; only shell-quoted assignments are prepended. A
  site name containing a quote, a `$(…)` or a backtick is inert data.

Two entries ship today, both WriteFreely — the same blog, headless or as a
writing machine:

| | |
|---|---|
| **WriteFreely** | 1 vCPU / 1 GB. Minimalist federated blogging behind Caddy with automatic HTTPS. Reach it from your own browser. |
| **WriteFreely Desktop** | 2 vCPU / 3 GB. The same blog *plus a machine to write on*: X, a kiosk window manager and a browser, booting straight into the editor already signed in as the admin you set up. Deliberately not GNOME — measured, `gnome-core` costs 802 additional packages to put one window on screen. |

From the terminal, no GUI needed:

```bash
vmx --appliances                       # the catalog, with each entry's fields
vmx --appliance-script "WriteFreely"   # just print the installer, build nothing

vmx --appliance "WriteFreely" --vm blog \
    WF_SITE_NAME="My Blog" WF_ADMIN_USER=matt
```

It waits for the first boot to finish and prints the appliance's real URL on
stdout. `WF_ADMIN_PASS` is left out above on purpose: password fields left blank
are generated from `crypto/rand` and written to `/root/` inside the guest, so
the happy path needs no typing and no reused password.

## Three ways in

vmxplore is tiered by what the host *can do* — probes, never licence checks.
Start anywhere; each layer unlocks more, and the top one you don't build at all.

| You have… | You get… | Effort |
|---|---|---|
| **stock Linux + KVM** | the full console: estate, serial + VNC consoles, lifecycle verbs, New VM | a few packages ([below](#1--stock-linux--kvm)) |
| **+ OpenZFS** | the storage join: clone lineage, snapshot classes, rollback, instant clones, golden → fleet | one repo + a pool ([below](#2--add-openzfs)) |
| **[kldload](https://kldload.com)** | *all of it, pre-wired* — plus clusters, Windows goldens, air-gap, mesh, eBPF | **zero — it's already done** ([below](#enhanced-on-kldload)) |

### 1 — stock Linux + KVM

Assume a clean, ordinary install with nothing set up. The KVM stack vmxplore
drives, the build toolchain, then turn it on.

**The KVM stack** (libvirt, `virt-install` for New VM/clone, `qemu-img` +
`xorriso` for the cloud-init seed):

```bash
# Fedora / RHEL / Rocky
sudo dnf install -y qemu-kvm libvirt virt-install libvirt-client qemu-img xorriso

# Debian / Ubuntu
sudo apt install -y qemu-system-x86 libvirt-daemon-system virtinst \
                    libvirt-clients qemu-utils xorriso

# Arch
sudo pacman -S --needed qemu-full libvirt virt-install xorriso
```

Optional: `guestfs-tools` (Fedora/Arch) / `libguestfs-tools` (Debian) adds
`virt-sysprep`, used by **Make Golden** to seal a template generically.

**The GUI build deps** (cgo + OpenGL; a headless box can skip these and
`make tui` for the static terminal binary only):

```bash
# Fedora / RHEL
sudo dnf install -y golang gcc pkgconf-pkg-config mesa-libGL-devel \
     libX11-devel libXcursor-devel libXrandr-devel libXinerama-devel \
     libXi-devel libXxf86vm-devel wayland-devel libxkbcommon-devel fontconfig-devel

# Debian / Ubuntu
sudo apt install -y golang gcc pkg-config libgl1-mesa-dev xorg-dev \
     libwayland-dev libxkbcommon-dev libfontconfig1-dev

# Arch
sudo pacman -S --needed go gcc pkgconf libgl libxcursor libxrandr \
     libxinerama libxi wayland libxkbcommon fontconfig
```

**Turn KVM on and join the group** (distro-agnostic):

```bash
sudo systemctl enable --now libvirtd
sudo usermod -aG libvirt "$USER"          # then log out and back in
sudo virsh net-start default              # the NAT network New VM uses
sudo virsh net-autostart default
```

**Build & run:**

```bash
git clone https://github.com/vmxplore/vmxplore && cd vmxplore
make            # vmxplore (GUI+TUI) + vmx (static TUI-only)
sudo make install
```

```bash
vmxplore                 # the GUI
vmx --tui                # the terminal UI (any ssh session)
vmx --once               # print the estate table and exit (scripts)
vmx --connect HOST       # drive a remote hypervisor over qemu+ssh
```

That's it — a stock distro is now a hypervisor with a console. Everything above
is upstream KVM/libvirt; vmxplore only adds the window.

#### Preflight — are you ready?

Confirm each before (or right after) building. vmxplore surfaces these as clear
messages rather than cryptic failures, but checking up front is faster:

- [ ] **CPU virtualization on** (BIOS/UEFI VT-x / AMD-V):
      `grep -Eqc 'vmx|svm' /proc/cpuinfo && echo ok`
- [ ] **KVM device present**: `test -e /dev/kvm && echo ok`
- [ ] **libvirt running**: `systemctl is-active libvirtd`
- [ ] **You're in the `libvirt` group** (log out/in after adding):
      `id -nG | grep -qw libvirt && echo ok`
- [ ] **System libvirt reachable** (this is the estate vmxplore reads):
      `virsh -c qemu:///system list --all`
- [ ] **Default network active** (New VM attaches to it): `virsh net-list --all`
- [ ] **Build tools present** (GUI only): `go version && pkg-config --exists gl && echo ok`

```bash
for c in "grep -Eqc 'vmx|svm' /proc/cpuinfo" "test -e /dev/kvm" \
         "systemctl is-active --quiet libvirtd" "id -nG | grep -qw libvirt"; do
  eval "$c" && echo "ok   : $c" || echo "FIX  : $c"
done
```

### 2 — add OpenZFS

The storage join lights up the moment `zfs` is present and your VMs sit on
zvols. **You don't need ZFS on root** — just a pool with a dataset for VM
volumes. Then clones are instant, lineage shows up, and golden → fleet works.

```bash
# Ubuntu — OpenZFS ships in the archive
sudo apt install -y zfsutils-linux

# Debian — enable contrib, then:
sudo apt install -y zfs-dkms zfsutils-linux

# Fedora / RHEL / Rocky / Arch — add the OpenZFS (or archzfs) repo first;
# it's a kernel module, so follow the current per-distro instructions:
#   https://openzfs.github.io/openzfs-docs/Getting%20Started/
```

Make a pool and a home for VM volumes (a spare disk — or a file, just to try it):

```bash
# a whole disk:
sudo zpool create rpool /dev/sdX
# …or kick the tires on a file-backed pool:
truncate -s 40G ~/vmpool.img && sudo zpool create vmpool "$PWD/vmpool.img"

sudo zfs create rpool/vms          # where vmxplore puts VM zvols
```

Now every VM on a zvol shows its used space, snapshot count, and clone lineage
in the tree; **Clone** and **EZ Fleet** become zero-copy `zfs clone` operations
that finish in seconds. Doing this *properly* across a fleet — ZFS on root,
reproducible, air-gapped, snapshot-everything — is exactly what kldload
automates for you (next).

### Enhanced on kldload

[**kldload**](https://kldload.com) is the reproducible, multi-distro substrate:
ZFS on root, an in-kernel WireGuard mesh, eBPF observability, and KVM — assembled
with opinions and wired together, so vmxplore's third tab turns into a command
center where the hard things are already done. Point vmxplore at a kldload box
(or open it right there) and, one tile away:

- **Spin up a cluster from nothing** — `kspawn` conjures a multi-node cluster of
  instant ZFS clones on its own encrypted WireGuard backplane; `kube-cluster`
  brings up Kubernetes-on-ZFS (Cilium eBPF, MetalLB, the ZFS CSI) in one command.
- **Golden images, everywhere** — `kimage` builds a sealed cloud-init golden;
  `kexport` ships any VM to **nine formats** (qcow2, raw, vhd, vmdk, ova, oci,
  lxc, firecracker, all) — feed them to Packer, any cloud, any hypervisor.
- **Windows, unattended** — `kvm-win` builds a Win11/Server golden fully
  hands-off: OVMF Secure Boot, TPM 2.0, virtio, even WSL2 nested — then clones
  it like any other golden.
- **Snapshot everything, undo anything** — every install and change snapshots
  first; roll back the whole transaction, not just a package.
- **Air-gapped and reproducible** — the image *is* the repo; provision a fleet
  from one USB with the network unplugged.
- **The rest of the estate** — a self-forming WireGuard mesh (no DNS, no IPs),
  live eBPF flow maps and tracing, GPU sharing, an on-box model — all on by
  default.

None of that is a vmxplore feature — it's what a **kernel-loaded substrate** can
do, and vmxplore is simply the window that makes it one click. The generic tool
recruits the user; kldload is what the console makes effortless.

## Remote

`--connect <host | user@host | qemu+ssh://host/system>` points the whole tool at
a headless hypervisor over ssh — the estate, the verbs, the ZFS join, the serial
and VNC consoles all follow. Same key and `known_hosts` as your shell; nothing
new to configure. (In the GUI, the **Connect** button on the estate header does
the same.)

## Grouping rules

Estate grouping and snapshot classification are data, not code: rules files
(embedded generic + kldload profiles, `/etc/vmxplore/rules` or `--rules` to
override). The core contains no site-specific strings.

## License

BSD-3-Clause. Part of the kldload family; built for [vmxplore.dev](https://vmxplore.dev).
