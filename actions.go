//go:build gui

// actions.go — the catalog of what each kldload tool can be asked to do.
//
// This is DATA, not behaviour: a table of tools, the verbs each one offers,
// the exact argv for every verb, and whether that verb builds something or
// destroys something. The tile launcher reads it to decide what to draw and
// what to run.
//
// WHY it is curated argv and not a command string: several k-tools treat
// argv[1] as a VM name, and kvm-create built a zvol literally called
// "--help" during testing. Every entry here is exact, prompt text is
// field-split, and nothing passes through a shell.
//
// WHY it is its own file: 157 lines of static table were sitting in the
// middle of runGUI, a 2300-line function, where they read as logic and made
// the surrounding code harder to find. Nothing in it closes over any GUI
// state, so it lifts out whole.
//
// Notes: curated from each tool's REAL usage banner (2026-08-07 sweep) — most
// k-tools are `tool <subcommand>` CLIs, so a bare tile just prints usage.
// %SEL% is substituted with the selected VM name at launch time.
package main

// Curated from each tool's REAL usage banner (2026-08-07 sweep) —
// most k-tools are `tool <subcommand>` CLIs, so a bare tile just
// prints usage. Tapping a cataloged tile opens a second tile page of
// its verbs; prompts collect the argument, %SEL% is the selected VM.
// WARN: several tools treat argv[1] as a VM name (kvm-create built a
// zvol called "--help" in testing) — argv here is exact and curated,
// prompt text is field-split, nothing passes through a shell.
type toolAction struct {
	label   string
	desc    string   // one line under the title — the tile explains itself
	argv    []string // %SEL% → selected VM name
	prompt  string   // non-empty: ask, append the fields to argv
	confirm bool     // destructive: show the exact argv first
	builds  bool     // creates something — tile renders green
	// needs is what the HOST must provide for this verb to work at all.
	// Zero value CapNone = always available, so every existing entry keeps
	// its behaviour and only the ones annotated below are gated.
	//
	// WHY: vmx ships standalone onto plain libvirt hosts. Without this, a
	// KVM-only user could press "images: base…" and watch klab fail on a
	// zvol path that was never going to exist on their machine. See
	// capabilities.go — the gate is a probe, never a "is this kldload" flag.
	needs Capability
}

var toolActions = map[string][]toolAction{
	"kube-cluster": {
		{label: "status", desc: "nodes, versions, IPs — the cluster right now", argv: []string{"kube-cluster", "status"}},
		{label: "bootstrap…", desc: "golden image + full cluster, one shot", builds: true, argv: []string{"kube-cluster", "bootstrap"},
			prompt: "options (e.g. --workers 3, empty = defaults)"},
		{label: "golden", needs: CapKlab, desc: "build the golden image only", builds: true, argv: []string{"kube-cluster", "golden"}},
		{label: "scale…", desc: "add worker nodes to the running cluster", builds: true, argv: []string{"kube-cluster", "scale"},
			prompt: "how many workers to add"},
		{label: "destroy", desc: "tear down the cluster — VMs and zvols", argv: []string{"kube-cluster", "destroy"},
			confirm: true},
	},
	// The lab is a whole workflow, not one command: build goldens once, clone
	// them into a blue site, run the suite, stage changes in green and promote
	// when they pass. The verbs are ordered so the tile grid reads as that
	// process — build, deploy, test, report, promote.
	//
	// WHY THESE HANG OFF klab AND NOT kzfs-lab/kzfs-test:
	// There were three ZFS lab tools and only one of them is real. klab builds
	// klab-golden-<distro> (blue/green) and klab-ztest-<distro> (the OpenZFS
	// test lineage); kldload-autodeploy runs it, and the web console reports on
	// it. kzfs-lab looks for zfslab-golden-* and kzfs-test for
	// kzfstest-golden-* — VM names nothing has ever created.
	//
	// This tile used to drive kzfs-lab, so an operator who built the ZFS test
	// goldens watched them appear in the estate panel while the tool beside it
	// reported "Not built: 6" forever (.131, 2026-08-15). Same screen, two
	// different lineages. Pointing the workflow at klab makes the tile describe
	// the machine it is running on.
	"klab": {
		{label: "status", desc: "every golden, site and clone in the lab", argv: []string{"klab", "status"}},
		{label: "results", desc: "what the last test run found", argv: []string{"klab", "results"}},

		// ── Images ──────────────────────────────────────────────────────
		// One entry per image SET, then one that builds every set. These are
		// the same images the installer's "Build all images" tile produces;
		// skipping it at install left no way to build them afterwards except
		// knowing the klab verbs by heart (operator, 2026-08-19).
		//
		// Each set is a distinct lineage, not a variation:
		//   base     — lean blue/green images, what clones and kspawn clone from
		//   desktop  — the same distros carrying GNOME, for workstation clones
		//   ztest    — every zfs-tests.sh prerequisite and pre-created loopback
		//              vdevs baked in, which the lean images deliberately omit
		{label: "images: base…", needs: CapKlab, desc: "the lean blue/green base images, one per distro", builds: true,
			argv: []string{"klab", "golden"}, prompt: "distro or all (centos rocky fedora debian ubuntu)"},
		{label: "images: desktop…", needs: CapKlab, desc: "GNOME desktop images — for workstation clones", builds: true,
			argv: []string{"klab", "golden-desktop"}, prompt: "distro or all (centos rocky fedora debian ubuntu)"},
		{label: "images: ztest…", needs: CapKlab, desc: "OpenZFS test images — zfs-tests.sh prerequisites baked in", builds: true,
			argv: []string{"klab", "golden-ztest"}, prompt: "distro or all (centos rocky fedora debian ubuntu)"},
		// build-all is sequential and single-flight by design: it builds every
		// golden, then deploys blue, then green. Idempotent — a re-run after a
		// partial failure picks up where the last one stopped.
		{label: "images: BUILD ALL", needs: CapKlab, desc: "every image set, then the blue and green sites — hours", builds: true,
			argv: []string{"klab", "build-all"}, confirm: true},
		{label: "deploy blue", needs: CapKlab, desc: "clone the goldens into the blue site", builds: true, argv: []string{"klab", "deploy", "blue"}},
		{label: "deploy green", needs: CapKlab, desc: "clone the goldens into green — the staging site", builds: true, argv: []string{"klab", "deploy", "green"}},
		{label: "test (quick)", desc: "the fast pass — about 5 minutes per distro", argv: []string{"klab", "test", "--quick"}},
		{label: "test…", desc: "the complete zfs-tests.sh suite — hours per distro", argv: []string{"klab", "test"},
			prompt: "flags (e.g. --distro centos, empty = all, full run)"},
		{label: "verify", desc: "check the lab is sane before trusting a run", argv: []string{"klab", "verify"}},
		{label: "promote green", desc: "green becomes blue — blue is snapshotted first", argv: []string{"klab", "promote", "green"},
			confirm: true},
		{label: "rollback", desc: "revert blue to its previous snapshot", argv: []string{"klab", "rollback"},
			confirm: true},
		{label: "destroy…", desc: "tear down a site; goldens are preserved unless you say goldens", argv: []string{"klab", "destroy"},
			prompt: "blue, green, all or goldens", confirm: true},
	},
	"kspawn": {
		{label: "list", desc: "every spawned cluster", argv: []string{"kspawn", "list"}},
		{label: "spawn…", needs: CapKlab, desc: "instant multi-node cluster from klab goldens", builds: true, argv: []string{"kspawn", "spawn"},
			prompt: "flags (see kspawn spawn -h; empty = defaults)"},
		{label: "status…", desc: "one cluster's live state", argv: []string{"kspawn", "status"},
			prompt: "cluster name"},
		{label: "ssh…", desc: "straight into a cluster node", argv: []string{"kspawn", "ssh"},
			prompt: "cluster [node N]"},
		{label: "destroy…", desc: "cluster and all its zvols, gone", argv: []string{"kspawn", "destroy"},
			prompt: "cluster name", confirm: true},
	},
	"kimage": {
		{label: "build", desc: "prep this system as a cloud-init golden", builds: true, argv: []string{"kimage", "build"}},
		{label: "export qcow2", desc: "golden as qcow2", builds: true, argv: []string{"kimage", "export", "qcow2"}},
		{label: "export raw", desc: "golden as raw disk", builds: true, argv: []string{"kimage", "export", "raw"}},
		{label: "export vhd", desc: "golden as VHD", builds: true, argv: []string{"kimage", "export", "vhd"}},
		{label: "export vmdk", desc: "golden as VMDK", builds: true, argv: []string{"kimage", "export", "vmdk"}},
		{label: "export all", desc: "golden in every format", builds: true, argv: []string{"kimage", "export", "all"}},
		{label: "deploy…", desc: "clone N VMs out of an image", builds: true, argv: []string{"kimage", "deploy"},
			prompt: "<image> <count>"},
		{label: "full…", desc: "build + export + deploy, one shot", builds: true, argv: []string{"kimage", "full"},
			prompt: "[count] (empty = 1)"},
	},
	// kexport takes a raw disk — for the selected VM that's its zvol
	// (%DS% → /dev/zvol/<dataset>), so every format is one click.
	// Sealed by default (portable golden: no machine-id/host keys).
	"kexport": {
		{label: "VM → qcow2 (KVM/Proxmox)", desc: "KVM / Proxmox / OpenStack, compressed", builds: true, argv: []string{"kexport", "%DS%", "qcow2"}},
		{label: "VM → raw (dd)", desc: "dd-ready sparse disk image", builds: true, argv: []string{"kexport", "%DS%", "raw"}},
		{label: "VM → vhd (Azure/Hyper-V)", desc: "Azure / Hyper-V", builds: true, argv: []string{"kexport", "%DS%", "vhd"}},
		{label: "VM → vmdk (VMware)", desc: "VMware ESXi / vSphere", builds: true, argv: []string{"kexport", "%DS%", "vmdk"}},
		{label: "VM → ova (portable)", desc: "VMware / VirtualBox portable", builds: true, argv: []string{"kexport", "%DS%", "ova"}},
		{label: "VM → oci (docker/podman)", desc: "docker load / podman load tarball", builds: true, argv: []string{"kexport", "%DS%", "oci"}},
		{label: "VM → lxc template", desc: "LXC template tarball", builds: true, argv: []string{"kexport", "%DS%", "lxc"}},
		{label: "VM → firecracker", desc: "kernel + rootfs + config.json", builds: true, argv: []string{"kexport", "%DS%", "firecracker"}},
		{label: "VM → ALL VM formats", desc: "qcow2+raw+vhd+vmdk+ova in one run", builds: true, argv: []string{"kexport", "%DS%", "all"}},
	},
	"kvm-win": {
		{label: "golden win11", desc: "unattended EVAL install → sealed golden", builds: true, argv: []string{"kvm-win", "golden", "win11"}},
		{label: "golden win11 + WSL", desc: "Win11 golden with WSL2 baked in", builds: true, argv: []string{"kvm-win", "golden", "win11", "--wsl"}},
		{label: "golden server", desc: "Windows Server golden", builds: true, argv: []string{"kvm-win", "golden", "server"}},
		{label: "create…", desc: "instant clone of a Windows golden", builds: true, argv: []string{"kvm-win", "create"},
			prompt: "NAME --os win11|server [--ram MB] [--cpus N]"},
	},
	"ksnap": {
		{label: "snapshot all", desc: "snapshot all key datasets now", builds: true, argv: []string{"ksnap"}},
		{label: "list", desc: "every snapshot on the host", argv: []string{"ksnap", "list"}},
		{label: "rollback…", desc: "path back to its last snapshot", argv: []string{"ksnap", "rollback"},
			prompt: "path", confirm: true},
		{label: "destroy…", desc: "drop one snapshot", argv: []string{"ksnap", "destroy"},
			prompt: "snapshot name", confirm: true},
	},
	"kvm-clone": {
		{label: "clone selected VM…", desc: "zero-copy — shares blocks until it diverges", builds: true, argv: []string{"kvm-clone", "%SEL%"},
			prompt: "name for the new VM"},
	},
	"kvm-create": {
		{label: "create…", desc: "fresh zvol + virt-install", builds: true, argv: []string{"kvm-create"},
			prompt: "new VM name"},
	},
	"kvm-snap": {
		{label: "snapshot selected VM", desc: "crash-consistent zvol snapshot", builds: true, argv: []string{"kvm-snap", "%SEL%"}},
	},
	"kvm-delete": {
		{label: "delete selected VM", desc: "undefine the domain, remove its storage", argv: []string{"kvm-delete", "%SEL%"},
			confirm: true},
	},
} // kvm-demo and kube-demo are their own interactive menus — bare tiles

// toolDesc is the one-liner on each top-level tile — a screenshot of
// this grid should explain the product on its own.
var toolDesc = map[string]string{
	// The three "lab" tools are easy to confuse, so each says what it BUILDS.
	"klab":             "test bay: 5 distros, blue/green clones, pluggable payloads",
	"kube-cluster":     "Kubernetes on ZFS: bootstrap, scale, status",
	"kspawn":           "instant multi-node clusters from ZFS clones",
	"kvm-create":       "new VM on a fresh zvol — cloud image or your own ISO",
	"kvm-clone":        "instant copy-on-write clone of a VM",
	"kvm-delete":       "remove a VM and its storage",
	"kvm-snap":         "snapshot a VM's zvol",
	"kvm-list":         "every VM with state, RAM and ZFS usage",
	"kimage":           "golden cloud-init images: build, export, deploy",
	"kexport":          "ship the selected VM anywhere — 9 formats, sealed",
	"kvm-win":          "Windows goldens: unattended, virtio, TPM, WSL",
	"ksnap":            "host-level ZFS snapshots and rollback",
	"kvm-demo":         "guided KVM / ZFS / GPU showcase",
	"kube-demo":        "guided Kubernetes-on-ZFS showcase",
	"zxplore":          "the ZFS console: pools, datasets, snapshots, clones",
	"wgx":              "the WireGuard console: hosts, interfaces, peers",
	"kst-dashboard":    "live host dashboard — pools, capacity, services",
	"kldload-sysdiag":  "observability cockpit: disk, ZFS, network, kernel",
	"kldload-doctor":   "health checks with the fix for what they find",
	"kbe":              "boot environments: list, create, activate, roll back",
	"kldload-snapshot": "host-level snapshots of the whole system",
	"krecovery":        "the way back when a boot goes wrong",
	"kldload-console":  "the cluster cockpit — klab and Kubernetes",
	"kube-init":        "bring up a Kubernetes control plane",
	"bob":              "the substrate's assistant, in a terminal",
	"kst":              "this host at a glance: pool health, capacity, build",
	"shell":            "a plain bash prompt, right here",
}
