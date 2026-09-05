# Changelog

## Unreleased

### Build all: one at a time, built big

- **Tiles build in series by default.** The memory-derived parallel count
  launched five builds on a 32 GB desktop; they ballooned together, the OOM
  killer took the desktop session apart and the run died at seven of twelve.
  Tiles that finished under contention took eleven and twelve minutes each,
  the first one alone under two. `VMX_BUILD_JOBS=N` still builds N at once,
  at catalog size.
- **Each tile is built with most of the host.** Cores less two (at most
  eight) and four times its catalog RAM (at most 8 GB, never more than the
  host can spare) for the duration of its first boot, then trimmed back to
  catalog size once it answers: memory live via the balloon, vCPUs live
  where the guest allows and at the next boot otherwise. The log says what
  it borrowed and when it gave it back.
- **Build all can be cancelled.** A Cancel button on the window, Ctrl-C on
  the CLI: nothing more starts and the tile in flight is removed, so the
  next run rebuilds it instead of skipping a VM whose first boot never
  finished. Before this the only way to stop a twelve-tile run was to close
  the program.
- **Finished tiles are shut off.** A build-all leaves VMs to start when
  wanted, not twelve services running at once on a host that may not have
  the memory for them. The report still carries every address and login;
  `VMX_BUILD_KEEP_RUNNING=1` leaves them up and opens their pages.
- **The closing report links each tile's upstream docs** next to the
  instance that was just built. The browser tabs at the end are the
  instances; the documentation is a line in the report.

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

- **One ssh policy, stated instead of inherited.** Every non-interactive ssh
  this tool runs — the ZFS reads and mutations, the console's `-L` forward —
  goes through one helper that sets `BatchMode=yes`,
  `StrictHostKeyChecking=accept-new` and `ConnectTimeout=10`. The ZFS path had
  no flags at all: it inherited whatever `~/.ssh/config` said, so a lab
  machine's global `StrictHostKeyChecking no` silently applied to the
  connection that runs `zfs destroy -r`, and a host that wanted a password hung
  the GUI on a prompt with no terminal to answer it.
- **Remote commands are shell-quoted.** `ssh host zfs snapshot NAME` does not
  exec an argv on the far side: ssh joins the arguments and the remote login
  shell re-parses them. So the local path (`exec.Command`, immune) and the
  remote path had *different* safety properties for the same argv, and
  `manual-x;reboot` was a command on the hypervisor. Remote commands now cross
  as single quoted words, and a new `validZFSName` gate rejects anything
  outside ZFS's own character set before it can reach an argv at all — the old
  check only looked for spaces, `@` and `/`.
- **The trust story is documented** in the README and a new man-page section,
  including the asymmetry that matters in practice: the estate connection is
  go-libvirt's own ssh (verifies against `~/.ssh/known_hosts`, fails closed,
  **ignores `~/.ssh/config`**) while everything else is the system `ssh` (which
  reads it). A host that only resolves through a config stanza needs its real
  `user@hostname` on `--connect`.
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

- **A remote target was honoured by the estate but not by the verbs.** Fifteen
  call sites hardcoded `qemu:///system` or ran a bare `zfs`, so under
  `--connect` they acted on the *local* machine while the domain lived on the
  remote one — a clone would snapshot there and copy here, and a "golden not
  found" answer silently downgraded a cheap clone to a full copy. Every virsh,
  virt-install, virt-clone and zfs call now routes through the target, and a
  test walks the source to fail the build if a new one does not.
- **Agentless guests showed no address.** Cloud images do not ship
  qemu-guest-agent, so the estate knew a VM was running but not where it
  landed. When the agent is absent, addresses now come from the hypervisor's
  own DHCP leases. The AGENT column still reports only the agent — it is a
  statement about the agent, not about whether an address was found.
- **A numeric VM name broke cloud-init entirely.** `instance-id: 11` is the
  integer eleven in YAML, so the NoCloud datasource never initialised and
  *nothing* in user-data applied — no user, no password, no first-boot script.
  The guest booted as a bare image with no error anywhere.
- **A data race** between `readLoop` and callers assigning the console's frame
  and clipboard callbacks, in shipped code. Found by the race detector the
  hour CI was added.
- **A second one, on the framebuffer size.** A guest resizing its display —
  which they do at boot — writes `fbW`/`fbH` from the read loop while the UI
  goroutine reads them on every mouse event, unsynchronised. Now behind
  accessors, and pinned by a test that talks to a fake RFB server, so
  `go test -race` covers it without needing a running guest: reverting the fix
  makes it fail.
- **A dead console now says so.** Every input write dropped its error, so a
  broken pipe left the last frame on screen while keystrokes and mouse moves
  went nowhere — indistinguishable from a frozen guest. Read and write errors
  land in one place, and the pane is replaced with "console disconnected: …"
  when the connection ends for any reason other than the operator detaching.
- **A stalled peer cannot wedge a console pane.** `DialTimeout` covers the TCP
  connect only, so a server that accepted and then said nothing hung the
  dialling goroutine forever; the handshake now runs under a deadline. TCP
  keepalive (30s/10s/3) detects a hypervisor that went away without closing.
  Deliberately *not* an idle read timeout: RFB is demand-driven and an idle
  desktop legitimately sends nothing for minutes.
- **The remote GPU probe never worked.** `ssh host sh -c <script>` let the
  remote login shell re-split a multi-line script, so `sh -c` received one word
  of it and the probe answered with shell syntax errors — swallowed, reported
  as "no GPUs". Remote hypervisors now probe like local ones.
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
- **`vmx --once` prints an IP column**, so a script or a cron check can answer
  "where did this VM land?" without a `virsh domifaddr` per domain.
- An **unwritable audit log says so once**, on stderr, instead of silently
  keeping no record while the tool claims every mutation is audited.
- The README and man page now state the console's two deliberate limits — Raw
  encoding only, and latin-1 clipboard from an agent-less guest — as the design
  choices they are rather than leaving them to be discovered.

### Project

- **CI**, mirroring zxplore: vet and test both flavors, race detector,
  staticcheck, govulncheck, mandoc lint, cross-compile matrix, and a
  version-guard that refuses a tag disagreeing with the version constant.
  It found the data race, dead code in the static build, and three deprecated
  APIs before it ever ran on a runner.
- **`golang.org/x/crypto` 0.48.0 → 0.54.0**, closing seven advisories this
  code actually reaches (`ConnectSystem → ConnectToURI → ssh.Dial`, the path
  every remote connection takes), and matching zxplore so the two consoles do
  not drift on a shared dependency. Zero reachable advisories now.
- `install.sh` for a from-source install.
- README callouts are generated from data, so a new screenshot does not mean
  redrawing them by hand.

## 0.3.0

Appliance catalog, EZ Fleet, the kldload tool launcher, GPU probing, and the
`qemu+ssh` remote path.
