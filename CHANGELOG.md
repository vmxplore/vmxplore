# Changelog

## 0.4.0 — 2026-08-11

Nine cloud images, a desktop selector, and the security work that should have
been there from the start.

### Push-button desktops

- **Pick GNOME, KDE or XFCE** in New VM or EZ Fleet and get a login screen.
  Cloud images are headless by design; the desktop installs on first boot and
  the guest reboots into it.
- **Fifteen verified combinations** — five distros × three desktops. Every
  package group was read off the distribution's own repository in a container
  rather than written from memory, which is how it was caught that Fedora's
  GNOME is `workstation-product-environment` (not the `-product` core, which
  ships a shell with no terminal), that its KDE group pulls **no display
  manager at all**, and that Arch's desktops are pacman *groups* invisible to
  `pacman -Si`.
- **Unverified pairs are not offered.** The menu is generated from the same
  table that installs them, so it cannot promise something a repository will
  not satisfy. Rocky, Alma and Amazon are headless-only until checked.
- On a fleet the desktop is installed **once, on the golden** — every clone
  inherits it, so ten desktop VMs cost about what one does.

### Cloud images

- **Nine presets**: Fedora, Debian, Ubuntu, CentOS Stream, Rocky, AlmaLinux,
  Amazon Linux 2023, openSUSE Leap, Arch.
- **Every image is verified** against its vendor's own checksum manifest
  before it is written to disk — not a digest pinned in this repo, which
  would be wrong the day the vendor rebuilds. Handles the three formats the
  vendors actually publish, including Debian's SHA512.
- **A mismatch deletes the file.** The download path is also the cache, so one
  bad transfer would otherwise poison every later build.
- **RHEL** takes an image downloaded from the portal plus your login or
  activation key, and entitles the guest on first boot. Red Hat's guest images
  sit behind an authenticated CDN that answers an anonymous fetch with a login
  page and HTTP 200 — a naive preset would have written 4KB of HTML to a zvol.

### Security

- **Guest consoles bind loopback.** They were created with
  `--graphics vnc,listen=0.0.0.0` and no password, and the RFB client speaks
  only security type *None* — so nothing authenticated anything. A remote
  console now goes through an `ssh -L` forward on libvirt's own channel.
- **Guest passwords are hashed.** The cloud-init seed stays attached as a
  cdrom for the life of the guest; the login password was cleartext in it,
  readable on the hypervisor and by any user inside the guest. The seed is
  installed `0600`.
- **A failed build unwinds itself.** A `sudo` refusal partway through left an
  orphan zvol and a seed behind, and the retry then failed on "dataset already
  exists" looking like a different bug.
- Every dialog validates before it commits, and re-opens with your input
  intact rather than failing deep in the pipeline.

### Fixed

- **A numeric VM name broke cloud-init entirely.** `instance-id: 11` is the
  integer eleven in YAML, so the NoCloud datasource never initialised and
  *nothing* in user-data applied — no user, no password, no first-boot script.
  The guest booted as a bare image with no error anywhere.
- **A data race** between `readLoop` and callers assigning the console's frame
  and clipboard callbacks, in shipped code. Found by the race detector the
  hour CI was added.
- The console takes keyboard focus when it attaches, not only when clicked —
  a guest could show a working pointer and a dead keyboard at a login prompt.
- EZ Fleet waited a fixed 90 seconds before sealing the golden. A desktop
  takes five to ten minutes, so clones would have come off a half-built
  system. It watches the disk instead.

### Interface

- The **kldload tab is sectioned** — 27 tools in six groups instead of one
  flat wall, ordered by how often each is reached for.
- One tile size everywhere, and tile descriptions truncate so a grid is not
  ragged.
- The mark is violet, matching zxplore and wgxplore in one icon idiom.

### Project

- **CI**, mirroring zxplore: vet and test both flavors, race detector,
  staticcheck, govulncheck, mandoc lint, cross-compile matrix, and a
  version-guard that refuses a tag disagreeing with the version constant.
  It found the data race, dead code in the static build, and three deprecated
  APIs before it ever ran on a runner.
- `install.sh` for a from-source install.
- README callouts are generated from data, so a new screenshot does not mean
  redrawing them by hand.

## 0.3.0

Appliance catalog, EZ Fleet, the kldload tool launcher, GPU probing, and the
`qemu+ssh` remote path.
