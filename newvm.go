// newvm.go — the native New VM pipeline: cloud image → disk → cloud-init
// seed → virt-install → persistence.
//
// What it does, in order:
//  1. Resolves the image: an explicit path, a family cache hit
//     (/var/lib/klab/images first — kldload hosts already carry them),
//     or a download into /var/lib/vmxplore/images.
//  2. Builds the disk: a sparse zvol (raw) when a ZFS parent is known,
//     else a qcow2 under /var/lib/libvirt/images.
//  3. Writes a NoCloud seed ISO (mkisofs/xorriso, -volid cidata): user,
//     password, ssh key, growpart — the same shape the kldload webui and
//     klab emit, so guests behave identically.
//  4. virt-install --import with virtio disk/net/video and VNC graphics —
//     the exact flag set the webui settled on (qxl corrupts modern Wayland
//     guests; virtio-gpu does not).
//  5. Re-defines the domain from its own XML: some libvirt builds create
//     --import --noautoconsole domains TRANSIENT, which silently
//     self-undefine on first shutdown (the webui's hard-won CRITICAL fix).
//
// Why: "install KVM and vmxplore does the rest" has to include the first
// VM. This is the generic tier — no kldload tooling required; privileged
// steps go through sudo -n per command and everything is audit-logged.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CloudImage is one distro preset — URL and libvirt os-variant, lifted
// verbatim from klab's curated catalog (the family source of truth).
type CloudImage struct {
	URL     string
	Variant string
	// SumURL is the vendor's OWN checksum manifest, not a hash pinned here.
	// Every one of these image URLs is a "latest" pointer that moves when
	// the vendor rebuilds, so a hardcoded digest would be wrong within days
	// and the pressure would be to delete the check rather than chase it.
	// Fetching the manifest beside the image verifies the bytes actually
	// published for that URL, and keeps working across rebuilds.
	SumURL string
	// SumAlgo is "sha256" or "sha512" — Debian publishes SHA512SUMS while
	// everyone else publishes SHA256.
	SumAlgo string
}

// DefaultGuestPassword is what a cloud-mode VM gets when the operator gave
// neither a password nor an ssh key — the same one EZ Fleet has always
// baked into its clones, so there is one answer to "what's the password"
// across every path that builds a VM here.
const DefaultGuestPassword = "kldload"

var cloudImages = map[string]CloudImage{
	"fedora": {
		"https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2",
		"fedora44",
		"https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-44-1.7-x86_64-CHECKSUM",
		"sha256",
	},
	"debian": {
		"https://cloud.debian.org/images/cloud/trixie/daily/latest/debian-13-genericcloud-amd64-daily.qcow2",
		"debian12",
		"https://cloud.debian.org/images/cloud/trixie/daily/latest/SHA512SUMS",
		"sha512",
	},
	"debian-bookworm": {
		"https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
		"debian11",
		"https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS",
		"sha512",
	},
	"ubuntu": {
		"https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
		"ubuntu24.04",
		"https://cloud-images.ubuntu.com/noble/current/SHA256SUMS",
		"sha256",
	},
	"centos": {
		"https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2",
		"centos-stream9",
		"https://cloud.centos.org/centos/9-stream/x86_64/images/CHECKSUM",
		"sha256",
	},
	"rocky": {
		"https://dl.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2",
		"centos-stream9",
		"https://dl.rockylinux.org/pub/rocky/9/images/x86_64/CHECKSUM",
		"sha256",
	},
	"arch": {
		"https://geo.mirror.pkgbuild.com/images/latest/Arch-Linux-x86_64-cloudimg.qcow2",
		"archlinux",
		"https://geo.mirror.pkgbuild.com/images/latest/Arch-Linux-x86_64-cloudimg.qcow2.SHA256",
		"sha256",
	},
	// EL10 alongside Rocky — same ABI, different governance, and operators
	// have strong opinions about which one they want.
	"alma": {
		"https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2",
		"almalinux9",
		"https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/CHECKSUM",
		"sha256",
	},
	// The SUSE family was missing entirely, which made the catalogue read as
	// "Red Hat, Debian, or Arch" when the substrate has no such opinion.
	// Cloud-first by construction — cloud-init is native rather than a port,
	// so the one-touch fields work without any coaxing.
	//
	// WARN: the filename is version-pinned even though the path says
	// "latest", so this 404s the day Amazon publishes a new build. There is
	// no stable alias to point at. VMX_CATALOG_LIVE=1 is what catches it —
	// run it after any vendor bump.
	"amazon": {
		"https://cdn.amazonlinux.com/al2023/os-images/latest/kvm/al2023-kvm-2023.12.20260803.3-kernel-6.1-x86_64.xfs.gpt.qcow2",
		"almalinux9",
		"https://cdn.amazonlinux.com/al2023/os-images/latest/kvm/SHA256SUMS",
		"sha256",
	},
	"opensuse": {
		"https://download.opensuse.org/repositories/Cloud:/Images:/Leap_15.6/images/openSUSE-Leap-15.6.x86_64-NoCloud.qcow2",
		"opensuse15.6",
		"https://download.opensuse.org/repositories/Cloud:/Images:/Leap_15.6/images/openSUSE-Leap-15.6.x86_64-NoCloud.qcow2.sha256",
		"sha256",
	},
}

// NOT in the catalogue, and why — so the next person does not re-derive it:
//
//	rhel      Red Hat's KVM guest images are behind an authenticated CDN;
//	          an unauthenticated fetch returns a login page with HTTP 200,
//	          which is exactly the shape that would have shipped a broken
//	          preset. RHEL guests are built from a local ImagePath plus
//	          subscription-manager registration in the post-installer —
//	          see rhelPostInstall.
//	oracle    Oracle Linux publishes the image but no checksum manifest
//	          beside it (404). VerifyImage fails closed, so an entry with
//	          nothing to verify against cannot be added.
//	cachyos   No official cloud qcow2 found. Their ISOs are installer media,
//	          not cloud images, so it would need the install path instead.

// CloudDistros lists the presets in menu order.
func CloudDistros() []string {
	return []string{"fedora", "debian", "debian-bookworm", "ubuntu", "centos", "rocky", "alma", "amazon", "opensuse", "arch"}
}

// NewVMSpec is everything the dialog collects. Two build modes:
//   - cloud   (ISOPath == ""): import a cloud image + cloud-init seed,
//     boots straight to a configured guest — the fast path.
//   - install (ISOPath != ""): blank disk + installer ISO, boots the
//     distro's own installer in the Screen tab. You run apt/dnf/pacman
//     the normal way; no cloud-init, no preset user.
type NewVMSpec struct {
	Name      string
	Distro    string // cloud preset key; used only in cloud mode
	ImagePath string // explicit cloud qcow2/raw; wins over Distro (cloud mode)
	ISOPath   string // installer ISO — switches to install mode
	OSVariant string // libvirt os-variant for install mode (best-effort)
	VCPUs     int
	RAMMB     int
	DiskGB    int
	// DataGB attaches a SECOND, blank virtio disk. The appliance substrate
	// turns it into the guest's own pool (app_pool_init), which is what makes
	// a recipe's dataset tuning real. Zero = no data disk. It stays blank on
	// purpose: app_pool_init refuses anything that carries a signature.
	DataGB   int
	User     string // cloud mode only
	Password string // cloud mode only
	SSHKey   string // cloud mode only — one authorized_keys line
	// RootSSHKeys authorizes root directly — set only by the kldload
	// enrollment path, which needs root in the guest for kvm-mesh and the
	// cert push. Ordinary VMs never get one.
	RootSSHKeys []string
	PostInst    string // cloud mode only — bash run as root on first boot
	Desktop     string // cloud mode only — "", "none", "gnome", "kde", "xfce"
	Sound       bool   // wire the guest's card to the host's audio session
	UEFI        bool   // boot via OVMF instead of SeaBIOS
	TPM         bool   // emulated TPM 2.0 (swtpm) — Windows 11 requires one
	DriverISO   string // second CD-ROM, e.g. virtio-win for Windows installs

	// RHEL entitlement. Red Hat's KVM guest images sit behind an
	// authenticated CDN, so unlike every other preset the IMAGE cannot be
	// fetched for you — point ImagePath at one downloaded from the Red Hat
	// portal. What these do is register the GUEST once it boots, which is
	// what turns a RHEL image into a RHEL machine that can install
	// anything. Same two auth methods the installer accepts: portal
	// username/password, or activation key + organisation ID.
	//
	// Only needed for the golden image. Clones inherit the entitlement.
	RHELUser string
	RHELPass string
	RHELKey  string // activation key
	RHELOrg  string // organisation ID
}

var vmNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// runStep executes one pipeline command: sudo -n when root work is needed
// and we aren't root, the exact argv echoed to progress, the run
// audit-logged with its exit code — the same contract as the verb plans.
// stepLabel turns a pipeline command into a line worth showing a person.
//
// The status bar used to echo raw argv, which read as debug output and
// told the operator nothing they could act on. The exact command still
// goes to the audit log, where it belongs and where it can be replayed;
// this is the narration. An empty result means the step is plumbing not
// worth a line (mkdir, cp).
//
// Note this is deliberately NOT applied to the verb confirmation dialog:
// there, showing the precise command before asking is the safety
// feature, not noise.
func stepLabel(argv []string) string {
	for len(argv) > 0 && (argv[0] == "sudo" || argv[0] == "-n") {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return ""
	}
	sub := ""
	if len(argv) > 1 {
		sub = argv[1]
	}
	switch argv[0] {
	case "zfs":
		if sub == "create" {
			return "creating the disk"
		}
		return "storage: " + sub
	case "qemu-img":
		if sub == "resize" {
			return "sizing the disk"
		}
		return "writing the cloud image to the disk"
	case "curl":
		return "downloading the cloud image (once — cached after)"
	case "mkisofs", "xorriso", "genisoimage":
		return "building the cloud-init seed"
	case "virt-install":
		return "creating the VM"
	case "virsh":
		if sub == "-c" || sub == "--connect" {
			return "defining the VM so it survives a shutdown"
		}
		return "libvirt: " + sub
	case "mkdir", "cp", "rm":
		return "" // plumbing
	}
	return argv[0]
}

func runStep(progress func(string), root bool, argv ...string) error {
	if root && os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	if label := stepLabel(argv); label != "" {
		progress(label)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	rc := 0
	if err != nil {
		rc = 1
	}
	auditLog(strings.Join(argv, " "), rc)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", argv[0], msg)
	}
	return nil
}

func (s NewVMSpec) install() bool { return s.ISOPath != "" }

func (s NewVMSpec) validate() error {
	if !vmNameRE.MatchString(s.Name) {
		return fmt.Errorf("VM name must be alphanumeric with . _ - only")
	}
	if s.VCPUs < 1 || s.RAMMB < 256 || s.DiskGB < 2 {
		return fmt.Errorf("need at least 1 vcpu, 256 MB RAM, 2 GB disk")
	}
	if s.install() {
		// install mode: just need the ISO to exist; the guest's own
		// installer collects everything else (user, packages, layout)
		if _, err := os.Stat(s.ISOPath); err != nil {
			return fmt.Errorf("installer ISO: %v", err)
		}
		return nil
	}
	// cloud mode
	if s.ImagePath == "" {
		if _, ok := cloudImages[s.Distro]; !ok {
			return fmt.Errorf("pick a distro preset, an image, or an installer ISO")
		}
	} else if _, err := os.Stat(s.ImagePath); err != nil {
		return fmt.Errorf("image: %v", err)
	}
	if s.User == "" || strings.ContainsAny(s.User, " :") {
		return fmt.Errorf("user must be a plain unix name")
	}
	return nil
}

// imageCaches is the lookup order for already-downloaded cloud images —
// the family caches first so a kldload host never downloads twice.
var imageCaches = []string{
	"/var/lib/klab/images",
	"/var/lib/kube-cluster/images",
	"/var/lib/vmxplore/images",
}

func cachedImage(distro, url string) string {
	base := filepath.Base(url)
	var candidates []string
	if distro != "" {
		// the webui's cache names by distro, not by URL basename
		candidates = append(candidates,
			"/var/lib/kldload/cloud-init/"+distro+"-cloud.qcow2")
	}
	for _, d := range imageCaches {
		candidates = append(candidates, filepath.Join(d, base))
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return p
		}
	}
	return ""
}

// isoTool picks the NoCloud seed builder present on this host.
func isoTool(out, ud, md string) ([]string, error) {
	if _, err := exec.LookPath("mkisofs"); err == nil {
		return []string{"mkisofs", "-output", out, "-volid", "cidata",
			"-joliet", "-rock", ud, md}, nil
	}
	if _, err := exec.LookPath("genisoimage"); err == nil {
		return []string{"genisoimage", "-output", out, "-volid", "cidata",
			"-joliet", "-rock", ud, md}, nil
	}
	if _, err := exec.LookPath("xorriso"); err == nil {
		return []string{"xorriso", "-as", "mkisofs", "-output", out,
			"-volid", "cidata", "-joliet", "-rock", ud, md}, nil
	}
	return nil, fmt.Errorf("no mkisofs/genisoimage/xorriso — install one for cloud-init seeds")
}

// yamlQuote renders s as a YAML double-quoted scalar.
//
// WHY every operator-supplied value goes through this: a bare scalar is
// safe only until it contains YAML punctuation, and these values come
// from the operator's keyboard and their ~/.ssh. An ssh key whose comment
// carries a colon — "ek-debug: dev login to appliances", which is what a
// real key on this host looks like — parses as a nested MAPPING, not a
// string. cloud-init then rejects the whole users block and the key is
// never installed, silently: the VM boots, the app works, and ssh answers
// "Permission denied (publickey)" forever. A password containing a comma
// or a brace breaks the chpasswd flow mapping the same way.
// HISTORY: wf-desktop, 2026-08-09 — `cloud-init schema --system` reported
// users.0.ssh_authorized_keys.0 "is not of type 'string'".
//
// Double quotes are the one YAML style with defined escaping for every
// printable character; backslash and quote are the only two needing it.
// Newlines are rejected upstream, since these land in line-oriented
// config.
func yamlQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// vdagentArg gives every VM a real clipboard.
//
// Without it, "paste" into a guest can only mean synthesising keystrokes —
// which is fragile in anything running JavaScript, mangles anything needing
// a modifier, and turns a newline into a form submission. With it, qemu
// carries the VNC client's cut text over a virtio-serial port to
// spice-vdagent inside the guest, which sets the guest's own clipboard.
// Paste then behaves the way paste behaves: instant, atomic, and ctrl+V
// inside the guest works on its own.
//
// The guest half is the spice-vdagent package; a guest without it simply
// ignores the port, so this costs nothing on machines that never install
// it and the keystroke fallback still covers them.
//
// mouse.mode=client is the same channel's other job — absolute pointer
// positioning, so the guest cursor tracks the host cursor instead of
// drifting.
const vdagentArg = "qemu-vdagent,source.clipboard.copypaste=on," +
	"source.mouse.mode=client,target.type=virtio,target.name=com.redhat.spice.0"

// gaChannelArg is the TRANSPORT for qemu-guest-agent. Without it the agent
// daemon runs happily inside the guest and can talk to nobody: the host has no
// virtio path to reach it, so `virsh domifaddr --source agent`, guest-initiated
// graceful shutdown and fs-freeze all fail as though the agent were absent.
//
// HISTORY: 2026-08-15, VM "fed". It had the vdagent channel above (clipboard
// worked) and no guest-agent channel, so `--source agent` returned empty and
// the VM's IP could only be found via the DHCP lease table. Installing the
// guest package is only half the job; this is the other half, and the half
// that is easy to forget because the guest side looks correct.
//
// fs-freeze is the one that matters most here: it is what makes a snapshot of
// a RUNNING guest filesystem-consistent instead of crash-consistent.
const gaChannelArg = "unix,target.type=virtio,target.name=org.qemu.guest_agent.0"

// videoArg is the virtio-gpu device every VM gets.
//
// vram is in KiB. Measured, not assumed: a guest reached 2560x1440 on
// the libvirt default, because virtio-gpu allocates its framebuffer from
// guest RAM rather than from a fixed VGA aperture. 64 MB is stated
// anyway as headroom for the modes above that and for the VGA-compat
// path a pre-DRM boot uses, and it costs nothing on a host with
// gigabytes.
//
// Applies to newly created VMs only; an existing domain keeps whatever
// its XML already says.
const videoArg = "model.type=virtio,model.vram=65536"

// userData renders the #cloud-config the seed carries. Same behaviours as
// the family tools: named sudo user, password auth on, growpart.
func userData(s NewVMSpec) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", yamlQuote(s.Name))
	// Everything cloud-init does goes to BOTH consoles as well as the log,
	// so the build narrates itself wherever the operator is watching
	// instead of showing a login prompt above ten silent minutes of apt.
	// /dev/console is the serial port on a cloud image (console=ttyS0 is
	// baked into its cmdline and cannot be changed before the first boot);
	// /dev/tty0 is the VGA text console the Screen tab renders, which is
	// otherwise blank until X claims it. An appliance's first boot
	// installs packages, writes configs and sometimes reboots; watching
	// that happen is the difference between "it is working" and "it is
	// hung". The default only tees to /var/log/cloud-init-output.log,
	// which nobody can read until the machine they are waiting on is up.
	b.WriteString("output: {all: '| tee -a /var/log/cloud-init-output.log " +
		"/dev/tty0 > /dev/console'}\n")
	b.WriteString("ssh_pwauth: true\n")
	// A desktop is only reachable after a reboot: set-default changes the
	// NEXT boot, so the running system stays in multi-user and the operator
	// gets a text console forever. Verified on 192.168.122.124, 2026-08-11 —
	// GNOME fully installed, gdm enabled, graphical.target set as default,
	// and the machine still sat at a login prompt.
	//
	// power_state is cloud-init's own mechanism and runs AFTER every module
	// finishes, which `systemctl isolate` from inside runcmd would not: that
	// isolates away the very unit the script is running under.
	if desktopPostInstall(s.Distro, s.Desktop) != "" {
		b.WriteString("power_state:\n  mode: reboot\n  condition: true\n" +
			"  message: 'vmxplore: desktop installed — rebooting into it'\n")
	}
	// The guest agent, on every machine this builds.
	//
	// WHY UNCONDITIONAL: gaChannelArg already gives every domain the virtio
	// transport, but nothing was ever installing the guest half, so all of
	// them carried a channel with nothing on the other end. libvirt then logs
	// "Guest agent is not responding" on every probe — 1,863 of them in ten
	// minutes on a host with eleven guests — and the estate falls back to DHCP
	// leases for addresses it should be able to ask the guest for directly.
	//
	// The package name is the same on dnf, apt and pacman, so this needs no
	// per-distro branch. cloud-init installs it before runcmd, which means a
	// post-install script can rely on it being there.
	//
	// It carries no identity of its own — machine-id and the ssh host keys are
	// what must not be cloned, and kldload-seal/virt-sysprep clear those when
	// a golden is sealed. So installing it on the golden is safe: every clone
	// inherits the package and mints its own identity on first boot.
	if len(s.RootSSHKeys) > 0 {
		// Cloud images ship root disabled; without this the seeded key gets
		// a "command=disabled" prefix and the enrollment ssh never works.
		b.WriteString("disable_root: false\n")
	}
	b.WriteString("packages:\n  - qemu-guest-agent\n")
	b.WriteString("growpart:\n  mode: auto\n  devices: ['/']\n")
	fmt.Fprintf(&b, "users:\n  - name: %s\n", yamlQuote(s.User))
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    lock_passwd: false\n")
	if s.SSHKey != "" {
		fmt.Fprintf(&b, "    ssh_authorized_keys:\n      - %s\n",
			yamlQuote(strings.TrimSpace(s.SSHKey)))
	}
	if len(s.RootSSHKeys) > 0 {
		b.WriteString("  - name: root\n    ssh_authorized_keys:\n")
		for _, k := range s.RootSSHKeys {
			fmt.Fprintf(&b, "      - %s\n", yamlQuote(strings.TrimSpace(k)))
		}
	}
	// A machine nobody can log into is a broken machine. cloud-init leaves
	// an account with lock_passwd:false and no password unusable at the
	// console AND over ssh, so when the operator supplied neither a
	// password nor a key we fall back to the family default rather than
	// building a VM with no way in.
	// HISTORY: wf-desk, 2026-08-09 — the appliance dialog's app fields and
	// its guest-login fields look alike, the guest ones were left empty,
	// and the finished VM could only be reached by destroying it.
	pw := s.Password
	if pw == "" && s.SSHKey == "" {
		pw = DefaultGuestPassword
	}
	if pw != "" {
		// The password goes in HASHED. A NoCloud seed is an ISO9660 image
		// that stays attached to the guest as a cdrom, so `type: text` put
		// the guest's admin password in cleartext in two places at once: on
		// the hypervisor under /var/lib/libvirt/images, and inside the guest
		// where any local user could mount /dev/sr0 and read it. A hash is
		// what /etc/shadow would have held anyway.
		// HISTORY: 2026-08-10 security pass. Falls back to cleartext ONLY
		// when no hashing tool exists, because a VM nobody can log into is
		// a broken VM — but it says so, loudly, in the rendered seed.
		if h, err := hashPassword(pw); err == nil {
			b.WriteString("chpasswd:\n  expire: false\n  users:\n")
			fmt.Fprintf(&b, "    - {name: %s, password: %s, type: hash}\n",
				yamlQuote(s.User), yamlQuote(h))
		} else {
			b.WriteString("# WARNING: no openssl/mkpasswd on the hypervisor, so this\n" +
				"# seed carries a CLEARTEXT password. Install either and rebuild.\n")
			b.WriteString("chpasswd:\n  expire: false\n  users:\n")
			fmt.Fprintf(&b, "    - {name: %s, password: %s, type: text}\n",
				yamlQuote(s.User), yamlQuote(pw))
		}
	}
	// the custom post-installer: the operator's bash, written to a script
	// and run once as root on first boot via runcmd — output lands in the
	// guest's /var/log/cloud-init-output.log. This is what turns a stock
	// cloud image into somebody's own appliance (then Make Golden → clone).
	post := s.PostInst
	// Order matters: entitle first (nothing installs without repos), then
	// the desktop, then the operator's own script — which may well want to
	// configure the desktop the step above just installed.
	// Before the desktop, so a desktop session finds the audio stack already
	// present rather than starting without one and needing a re-login.
	if s.Sound {
		if snd := soundPostInstall(s.Distro); snd != "" {
			post = snd + "\n" + post
		}
	}
	if d := desktopPostInstall(s.Distro, s.Desktop); d != "" {
		post = d + "\n" + post
	}
	if reg := rhelPostInstall(s); reg != "" {
		post = reg + "\n" + post
	}
	// Enable the agent explicitly rather than trusting the distro preset.
	// Fedora and EL enable it on install; Debian's package does not start it
	// until the next boot, and a golden that is sealed before then produces
	// clones whose agent has never run.
	agent := "# The agent may already be running, and on some images the unit is\n" +
		"# socket-activated rather than enableable. Neither is a failure, and\n" +
		"# this script runs under `set -e` ahead of the operator's own\n" +
		"# post-install — so an unswallowed error here would abort THAT.\n" +
		"systemctl enable --now qemu-guest-agent >/dev/null 2>&1 || true\n"
	post = agent + post

	if strings.TrimSpace(post) != "" {
		b.WriteString("write_files:\n")
		b.WriteString("  - path: /var/lib/vmxplore-postinstall.sh\n")
		b.WriteString("    permissions: '0755'\n")
		b.WriteString("    content: |\n")
		b.WriteString("      #!/usr/bin/env bash\n")
		b.WriteString("      set -Eeuo pipefail\n")
		for _, line := range strings.Split(post, "\n") {
			// 6-space indent puts each line inside the YAML block scalar
			b.WriteString("      " + line + "\n")
		}
		b.WriteString("runcmd:\n")
		b.WriteString("  - [ bash, /var/lib/vmxplore-postinstall.sh ]\n")
	}
	return b.String()
}

// BuildNewVM runs the pipeline. progress gets one line per step (safe to
// call from any goroutine); every external command is audit-logged with
// its exit code, exactly like the verb plans.
func BuildNewVM(s NewVMSpec, zfsParent string, progress func(string)) error {
	if err := s.validate(); err != nil {
		return err
	}
	run := func(root bool, argv ...string) error {
		return runStep(progress, root, argv...)
	}

	// Every resource created below registers its undo here. Disarmed by
	// rb.commit() once virt-install has defined the domain; until then any
	// return path unwinds what was made. See rollback.go.
	rb := &rollback{}
	defer rb.run(progress)

	// destroyZvol / removeFile are the two undo shapes this build needs.
	destroyZvol := func(ds string) func() {
		return func() {
			progress("cleanup: destroying orphaned zvol " + ds)
			_ = runStep(progress, true, zfsArgv("destroy", "-r", ds)...)
		}
	}
	removeFile := func(path string) func() {
		return func() {
			progress("cleanup: removing " + path)
			_ = runStep(progress, true, "rm", "-f", path)
		}
	}

	// ── install mode: blank disk + installer ISO, boot the installer ────
	// No cloud image, no seed — the guest's own installer runs in the
	// Screen tab and you drive apt/dnf/pacman the normal way. Any
	// installer ISO works: Debian, Fedora, an Arch live ISO, a RHEL DVD.
	if s.install() {
		var diskArg string
		if zfsParent != "" {
			ds := zfsParent + "/" + s.Name
			if err := run(true, zfsArgv("create", "-s", "-V",
				fmt.Sprintf("%dG", s.DiskGB), ds)...); err != nil {
				return err
			}
			// `zfs create` fails when the dataset exists, so reaching here
			// means this build made it and may safely destroy it.
			rb.add(destroyZvol(ds))
			diskArg = "path=/dev/zvol/" + ds + ",bus=virtio,format=raw"
		} else {
			f := "/var/lib/libvirt/images/" + s.Name + ".qcow2"
			// qemu-img create refuses an existing file, but check anyway:
			// registering a delete for someone else's disk is the one
			// mistake this whole mechanism must never make.
			fresh := fileAbsent(f)
			if err := run(true, "qemu-img", "create", "-f", "qcow2", f,
				fmt.Sprintf("%dG", s.DiskGB)); err != nil {
				return err
			}
			if fresh {
				rb.add(removeFile(f))
			}
			diskArg = "path=" + f + ",bus=virtio,format=qcow2"
		}
		osArg := []string{"--osinfo", "detect=on,require=off"}
		if s.OSVariant != "" {
			osArg = []string{"--os-variant", s.OSVariant}
		}
		argv := append([]string{"virt-install", "--connect", target.LibvirtURI,
			"--name", s.Name,
			"--memory", fmt.Sprint(s.RAMMB),
			"--vcpus", fmt.Sprint(s.VCPUs),
			"--disk", diskArg,
			"--cdrom", s.ISOPath}, osArg...)
		argv = append(argv, firmwareArgs(s)...)
		if s.DriverISO != "" {
			// A SECOND cdrom, not a replacement: WinPE needs the virtio
			// storage driver available while the installer media is still
			// mounted, or Windows setup shows an empty disk list and the
			// install dead-ends before it starts.
			argv = append(argv, "--disk",
				"path="+s.DriverISO+",device=cdrom")
		}
		argv = append(argv,
			"--network", "network=default,model=virtio",
			"--graphics", "vnc,listen=127.0.0.1",
			"--video", videoArg,
			"--channel", vdagentArg,
			"--channel", gaChannelArg)
		// Sound is appended rather than inlined because the backend half is
		// conditional on the host — see audio.go, where an unreachable
		// PipeWire is a qemu startup failure and not a silent guest.
		argv = append(argv, audioArgs(target)...)
		argv = append(argv, "--noautoconsole")
		if err := run(false, argv...); err != nil {
			return err
		}
		// The domain exists; its disk belongs to it now, not to this build.
		rb.commit()
		progress(s.Name + " created — open the Screen tab and run the installer")
		return nil
	}

	// 1 — the image
	img := s.ImagePath
	variant := "linux2022"
	if img == "" {
		ci := cloudImages[s.Distro]
		variant = ci.Variant
		if c := cachedImage(s.Distro, ci.URL); c != "" {
			progress("image: cache hit " + c)
			img = c
		} else {
			img = filepath.Join("/var/lib/vmxplore/images", filepath.Base(ci.URL))
			if err := run(true, "mkdir", "-p", "/var/lib/vmxplore/images"); err != nil {
				return err
			}
			progress("downloading " + s.Distro + " cloud image (once — cached after)")
			if err := run(true, "curl", "-L", "--fail", "-o", img, ci.URL); err != nil {
				return err
			}
			// Verify BEFORE the image is used, and before it becomes the
			// cache for every later build. A failure here deletes the file,
			// so a retry re-downloads instead of reusing bad bytes.
			if err := VerifyImage(img, ci, progress); err != nil {
				return fmt.Errorf("cloud image verification failed: %w", err)
			}
		}
	}

	// 2 — the disk
	var diskArg string
	if zfsParent != "" {
		ds := zfsParent + "/" + s.Name
		if err := run(true, zfsArgv("create", "-s", "-V",
			fmt.Sprintf("%dG", s.DiskGB), ds)...); err != nil {
			return err
		}
		rb.add(destroyZvol(ds))
		// WHY: `zfs create -V` returns before udev has published
		// /dev/zvol/<ds>. qemu-img does not care — it happily *creates* a
		// regular file at a missing path, so the convert below would land
		// a multi-GB image in devtmpfs (RAM) while the real zvol stayed
		// empty. The VM then boots off RAM, is stuck at the cloud image's
		// size, loses everything on host reboot, and has no snapshots or
		// clones. Wait for a real block device, and fail loudly instead.
		if err := waitZvolNode("/dev/zvol/"+ds, progress); err != nil {
			return err
		}
		if err := run(true, "qemu-img", "convert", "-O", "raw", img,
			"/dev/zvol/"+ds); err != nil {
			return err
		}
		diskArg = "path=/dev/zvol/" + ds + ",bus=virtio,format=raw"
	} else {
		f := "/var/lib/libvirt/images/" + s.Name + ".qcow2"
		// convert OVERWRITES silently, so the pre-existence check is what
		// stops a failed rebuild from deleting the disk it clobbered.
		fresh := fileAbsent(f)
		if err := run(true, "qemu-img", "convert", "-O", "qcow2", img, f); err != nil {
			return err
		}
		if fresh {
			rb.add(removeFile(f))
		}
		if err := run(true, "qemu-img", "resize", f,
			fmt.Sprintf("%dG", s.DiskGB)); err != nil {
			return err
		}
		diskArg = "path=" + f + ",bus=virtio,format=qcow2"
	}

	// 2b — the data disk, blank by design.
	//
	// HISTORY: DataGB sat on the Appliance struct for half a day, documented
	// and copied by nothing — the classic written-not-wired. Every NeedsZFS
	// recipe silently degraded to plain directories because app_pool_init
	// found no disk. Caught while writing the appliance smoke suite, before
	// any operator hit it.
	var dataDiskArg string
	if s.DataGB > 0 {
		if zfsParent != "" {
			dds := zfsParent + "/" + s.Name + "-data"
			if err := run(true, zfsArgv("create", "-s", "-V",
				fmt.Sprintf("%dG", s.DataGB), dds)...); err != nil {
				return err
			}
			rb.add(destroyZvol(dds))
			if err := waitZvolNode("/dev/zvol/"+dds, progress); err != nil {
				return err
			}
			dataDiskArg = "path=/dev/zvol/" + dds + ",bus=virtio,format=raw"
		} else {
			df := "/var/lib/libvirt/images/" + s.Name + "-data.qcow2"
			dfresh := fileAbsent(df)
			if err := run(true, "qemu-img", "create", "-f", "qcow2", df,
				fmt.Sprintf("%dG", s.DataGB)); err != nil {
				return err
			}
			if dfresh {
				rb.add(removeFile(df))
			}
			dataDiskArg = "path=" + df + ",bus=virtio,format=qcow2"
		}
		progress(fmt.Sprintf("data disk: %dG, blank — the guest makes its pool from it", s.DataGB))
	}

	// 3 — the NoCloud seed
	tmp, err := os.MkdirTemp("", "vmx-seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ud := filepath.Join(tmp, "user-data")
	md := filepath.Join(tmp, "meta-data")
	if err := os.WriteFile(ud, []byte(userData(s)), 0600); err != nil {
		return err
	}
	// QUOTED, both of them. Unquoted, a numeric VM name is parsed by YAML as
	// an INTEGER — instance-id: 11 is the number eleven, not the string "11"
	// — and cloud-init's NoCloud datasource never initialises, so nothing in
	// user-data is applied at all: no user, no password, no runcmd. The VM
	// boots as a bare image and looks like the seed was ignored.
	//
	// Found 2026-08-11 by diffing a working seed against a failing one: the
	// only meaningful difference between "www" (worked) and "11" (did not)
	// was the quoting the number forced. user-data escaped this because its
	// hostname goes through yamlQuote already.
	meta := metaData(s.Name)
	if err := os.WriteFile(md, []byte(meta), 0600); err != nil {
		return err
	}
	tmpISO := filepath.Join(tmp, s.Name+"-seed.iso")
	isoArgv, err := isoTool(tmpISO, ud, md)
	if err != nil {
		return err
	}
	if err := run(false, isoArgv...); err != nil {
		return err
	}
	seedISO := "/var/lib/libvirt/images/" + s.Name + "-seed.iso"
	// install -m0600, not cp: cp leaves the seed 0644 under a directory
	// every local account can read. The seed is a credential even hashed —
	// it also carries the operator's ssh key and any post-installer.
	//
	// WARN: this image stays attached as a cdrom for the life of the guest.
	// cloud-init only needs it on first boot, so ejecting it afterwards
	// would be strictly better; doing that safely needs a reliable
	// "cloud-init finished" signal from inside the guest, which is a
	// guest-agent dependency this does not yet take. Hashing the password
	// is what makes the persistence tolerable rather than a leak.
	seedFresh := fileAbsent(seedISO)
	if err := run(true, "install", "-m0600", tmpISO, seedISO); err != nil {
		return err
	}
	if seedFresh {
		rb.add(removeFile(seedISO))
	}

	// 4 — virt-install (the webui's exact device choices). The catalog's
	// os-variant can be newer than this host's osinfo-db (the klab
	// resolver problem — E2E caught fedora44 unknown on onyx): retry with
	// osinfo detection, which never hard-fails.
	installArgs := func(osinfo ...string) []string {
		argv := []string{"virt-install", "--connect", target.LibvirtURI,
			"--name", s.Name,
			"--memory", fmt.Sprint(s.RAMMB),
			"--vcpus", fmt.Sprint(s.VCPUs),
			"--disk", diskArg}
		if dataDiskArg != "" {
			argv = append(argv, "--disk", dataDiskArg)
		}
		argv = append(argv,
			"--disk", "path="+seedISO+",device=cdrom,bus=sata",
			"--import")
		argv = append(argv, osinfo...)
		argv = append(argv,
			"--network", "network=default,model=virtio",
			"--graphics", "vnc,listen=127.0.0.1",
			"--video", videoArg,
			"--channel", vdagentArg,
			"--channel", gaChannelArg)
		argv = append(argv, audioArgs(target)...)
		return append(argv, "--noautoconsole")
	}
	err = run(false, installArgs("--os-variant", variant)...)
	if err != nil && strings.Contains(err.Error(), "Unknown OS name") {
		progress("os-variant " + variant + " unknown to this host's osinfo-db — using detection")
		err = run(false, installArgs("--osinfo", "detect=on,require=off")...)
	}
	if err != nil {
		return err
	}
	// virt-install leaves a TRANSIENT domain running off the disk. Register
	// its teardown now so it unwinds BEFORE the disk (undos run newest
	// first) — otherwise `zfs destroy` hits a zvol still open by qemu and
	// fails with "dataset is busy", leaving both behind.
	rb.add(func() {
		progress("cleanup: destroying transient domain " + s.Name)
		_ = runStep(progress, false, append(virsh(), "destroy", s.Name)...)
	})

	// 5 — persistence: re-define from live XML (see banner)
	xmlOut, err := virshOut("dumpxml", s.Name)
	if err != nil {
		return fmt.Errorf("dumpxml after install: %v", err)
	}
	xf := filepath.Join(tmp, s.Name+".xml")
	if err := os.WriteFile(xf, xmlOut, 0600); err != nil {
		return err
	}
	if err := run(false, append(virsh(), "define", xf)...); err != nil {
		return err
	}
	// Host half of the audio wiring, after the domain is persistent and
	// before anyone starts it.
	//
	// NEVER fatal. The VM exists and works at this point; failing the whole
	// build because a sound backend could not be attached would trade a
	// working machine for a missing speaker. The operator is told, and the
	// domain keeps its card — pointed at nothing until this is retried.
	if s.Sound {
		if err := wireHostAudio(s.Name); err != nil {
			progress("host audio not wired: " + err.Error() +
				" — the guest has a card but no output")
		} else {
			progress("host audio wired — sound reaches this machine on next start")
		}
	}
	// Committed only here: everything up to the persistent define is still
	// this build's to unwind. A failure at dumpxml or define leaves a
	// running transient domain, so the cleanup destroys the disk out from
	// under it — which is correct, because a VM that does not survive a
	// host reboot is not the VM that was asked for.
	rb.commit()
	progress(s.Name + " is up — cloud-init finishes the first boot (1–3 min)")
	return nil
}

// waitZvolNode blocks until dev is a real block device, or fails.
//
// Args: dev is the /dev/zvol/<dataset> path; progress may be nil-safe here
// because callers always pass the pipeline's logger.
//
// Returns nil once dev resolves to a block device. Two distinct failures,
// both fatal and both worth telling apart in the message:
//   - the node never appeared (udev not running, or ZFS did not publish it)
//   - the path exists but is NOT a device — the signature of an earlier
//     run having written a plain file there, which must never be used as
//     a disk and must not be silently overwritten either.
//
// HISTORY: shipped without this, a New VM on a ZFS host raced udev and put
// the guest's whole disk in devtmpfs. Found 2026-08-09 by inspecting a
// built appliance: lsblk showed 3G (the cloud image) on a 10G request, and
// /dev/zvol/<ds> was a regular file while the zvol itself had USED=56K.
func waitZvolNode(dev string, progress func(string)) error {
	return waitZvolNodeFor(dev, 30*time.Second, progress)
}

// waitZvolNodeFor is waitZvolNode with the deadline exposed, so tests can
// exercise the timeout path without actually waiting for it.
func waitZvolNodeFor(dev string, timeout time.Duration,
	progress func(string)) error {
	deadline := time.Now().Add(timeout)
	warned := false
	for {
		fi, err := os.Stat(dev) // Stat follows the symlink udev creates
		switch {
		case err == nil && fi.Mode()&os.ModeDevice != 0:
			return nil
		case err == nil:
			return fmt.Errorf("%s exists but is not a block device (%s) — "+
				"refusing to use it as a disk; remove it and retry",
				dev, fi.Mode().String())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear within %s — is udev running?",
				dev, timeout)
		}
		if !warned {
			progress("waiting for " + dev + " to appear")
			warned = true
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ZFSVMParent derives where VM zvols live on this host from the estate
// itself — the most common parent among existing VM-backing datasets.
// Empty means "no ZFS home for VMs here" and the pipeline goes qcow2.
func ZFSVMParent(rows []Row) string {
	counts := map[string]int{}
	for _, r := range rows {
		if r.DS == nil {
			continue
		}
		if i := strings.LastIndexByte(r.DS.Name, '/'); i > 0 {
			counts[r.DS.Name[:i]]++
		}
	}
	best, n := "", 0
	for p, c := range counts {
		if c > n {
			best, n = p, c
		}
	}
	return best
}

// hashPassword turns a cleartext password into a crypt(3) SHA-512 hash for
// cloud-init's `chpasswd ... type: hash`.
//
// WHY shell out: Go's standard library has no crypt(3), and the alternative
// is pulling in a dependency to do what every Unix already ships. openssl is
// first because it is present on effectively every hypervisor; mkpasswd
// (whois package on Debian, expect on RHEL) is the fallback.
//
// Args:    pw  the cleartext password. Never logged, never echoed — note
//
//	that both tools take it on STDIN rather than argv, so it does
//	not appear in the process table.
//
// Returns: a "$6$salt$hash" string, or an error when no tool is available.
// Failure modes callers must handle: no hashing tool installed. The caller
// falls back to a cleartext seed with a warning rather than building a VM
// nobody can log into.
func hashPassword(pw string) (string, error) {
	for _, try := range [][]string{
		{"openssl", "passwd", "-6", "-stdin"},
		{"mkpasswd", "-m", "sha-512", "--stdin"},
	} {
		if _, err := exec.LookPath(try[0]); err != nil {
			continue
		}
		cmd := exec.Command(try[0], try[1:]...)
		cmd.Stdin = strings.NewReader(pw)
		out, err := cmd.Output()
		h := strings.TrimSpace(string(out))
		// A crypt hash always starts with $<id>$; anything else means the
		// tool printed usage or a prompt, which must not reach the seed.
		if err == nil && strings.HasPrefix(h, "$6$") {
			return h, nil
		}
	}
	return "", errors.New("no openssl or mkpasswd to hash the guest password")
}

// rhelPostInstall returns the cloud-init runcmd lines that register a RHEL
// guest, or "" when no entitlement was supplied.
//
// WHY this is not a catalogue entry like the others: Red Hat's KVM guest
// images are served from an authenticated CDN. An unauthenticated fetch
// returns a LOGIN PAGE with HTTP 200 and text/html — so a naive preset would
// have "downloaded" 4KB of HTML, written it to a zvol, and produced a VM
// that fails to boot with nothing pointing at the cause. Verified 2026-08-10.
//
// So RHEL takes the ImagePath route: the operator downloads the qcow2 from
// the portal once, and these credentials entitle the guest on first boot.
//
// Args:   s  the spec; reads only the RHEL* fields.
// Returns: bash for the post-installer, or "" when nothing was supplied.
// Failure modes callers must handle: none here — an unentitled RHEL guest
// still boots, it just cannot install packages, and the script says so in
// the guest's cloud-init log rather than failing the build.
//
// SAFETY: the credentials reach the guest through the cloud-init seed, which
// is written 0600 and carries a hashed login password. A subscription
// password cannot be hashed — it has to be usable — so it IS present in
// cleartext inside the guest's seed image. That is the same exposure the
// Red Hat installer has, and the reason to prefer an activation key: a key
// is scoped and revocable, a portal password is neither.
func rhelPostInstall(s NewVMSpec) string {
	var reg string
	switch {
	case s.RHELUser != "" && s.RHELPass != "":
		reg = fmt.Sprintf("subscription-manager register --username %s --password %s --auto-attach",
			shellQuote(s.RHELUser), shellQuote(s.RHELPass))
	case s.RHELKey != "" && s.RHELOrg != "":
		reg = fmt.Sprintf("subscription-manager register --activationkey %s --org %s",
			shellQuote(s.RHELKey), shellQuote(s.RHELOrg))
	default:
		return ""
	}
	return "" +
		"# RHEL entitlement (vmxplore). Without this the guest boots but can\n" +
		"# install nothing, which looks like a broken image rather than an\n" +
		"# unregistered one — so say which it is, in the log the operator reads.\n" +
		"if command -v subscription-manager >/dev/null 2>&1; then\n" +
		"  if " + reg + "; then\n" +
		"    echo 'vmxplore: RHEL guest registered'\n" +
		"  else\n" +
		"    echo 'vmxplore: RHEL registration FAILED — the guest has no repos' >&2\n" +
		"  fi\n" +
		"else\n" +
		"  echo 'vmxplore: no subscription-manager — is this actually a RHEL image?' >&2\n" +
		"fi\n"
}

// shellQuote wraps a value in single quotes for safe use in the generated
// post-installer. A subscription password is operator-supplied text that
// ends up inside a bash script; without this a quote or a semicolon in it
// would be shell syntax rather than a password.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// metaData renders the NoCloud meta-data for a VM name.
//
// Both values are QUOTED. Unquoted, a numeric name is parsed by YAML as an
// INTEGER — instance-id: 11 is the number eleven, not the string "11" — and
// cloud-init's NoCloud datasource never initialises. Nothing in user-data is
// then applied: no user, no password, no runcmd. The VM boots as a bare
// image and looks for all the world like the seed was ignored.
//
// Found 2026-08-11 by diffing a working seed against a failing one. The only
// meaningful difference between "www" (worked) and "11" (did not) was what
// the number did to the quoting. user-data escaped this because its hostname
// already went through yamlQuote.
func metaData(name string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n",
		yamlQuote(name), yamlQuote(name))
}
