// sysdiag.go — what this host can do, as a system-requirements report.
//
// What it does, in order:
//  1. probes the host: /dev/kvm, libvirt, ZFS and the VM parent dataset, and
//     every tool a tier needs (virt-install, virt-clone, kvm-mesh, kldload-ca,
//     kldload-db, klab, wg);
//  2. collects the facts a requirements screen shows — OS, kernel, CPU and its
//     virtualisation flag, memory, and the versions of libvirt, QEMU, ZFS and
//     WireGuard actually installed;
//  3. lays the three substrates side by side — bare KVM, KVM + ZFS, kldloadOS —
//     each with the requirements it needs and whether THIS host meets them,
//     and the appliance features each one lights up.
//
// WHY: the catalog colours tiles by what the host can do, and the old "what
// works where" chart told the operator which column they were in without
// saying why. A requirements screen says why: the exact probe that failed,
// the version that is there, and what the next tier would need.
//
// The report is data; sysdiag_ui.go draws it, and `vmx --sysdiag` prints it,
// so the probes are exercised on a headless box too.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SysFact is one key/value line of the facts block.
type SysFact struct{ Key, Value string }

// SysProbe is one capability check with the sentence that explains it.
type SysProbe struct {
	Name, Detail string
	OK           bool
}

// SysReq is one requirement of a tier and whether this host meets it.
type SysReq struct {
	Name string
	Met  bool
}

// SysTier is one substrate column.
type SysTier struct {
	Key, Label string
	Reqs       []SysReq
	Has        []bool // indexed like Sysdiag.Features
}

// Sysdiag is the whole report.
type Sysdiag struct {
	Host     string
	Tier     string // "kvm" | "kvm+zfs" | "kldload" — KldloadTier's vocabulary
	Summary  string // Tier()'s sentence
	Facts    []SysFact
	Probes   []SysProbe
	Features []string
	Tiers    []SysTier
}

// sysFeatures are the appliance capabilities the ladder compares, in the
// order a tile meets them: in-guest tuning first, host storage, then estate.
var sysFeatures = []string{
	"tuned in-guest datasets",
	"recordsize / quota tuning",
	"per-title media datasets",
	"USB radio/tuner passthrough",
	"zvol backing (sparse, fast)",
	"instant whole-VM clones",
	"whole-VM snapshot/rollback",
	"WireGuard management mesh",
	"estate CA: trusted TLS",
	"Ansible inventory, automatic",
}

// sysTierHas is which features each substrate lights up. A tier's row is
// the previous tier's plus its own; the numbers are the index into
// sysFeatures where that tier's additions begin.
func sysTierHas(from int) []bool {
	out := make([]bool, len(sysFeatures))
	for i := range out {
		out[i] = i < from
	}
	return out
}

// sysTierReqs names what each tier needs, by probe name, so a requirement's
// "met" is exactly one probe's verdict and the two can never disagree.
var sysTierReqs = []struct {
	key, label string
	reqs       []string
	features   int
}{
	{"kvm", "bare KVM", []string{"KVM", "libvirt", "virt-install"}, 4},
	{"kvm+zfs", "KVM + ZFS", []string{"KVM", "libvirt", "virt-install", "ZFS", "virt-clone"}, 7},
	{"kldload", "kldloadOS", []string{"KVM", "libvirt", "virt-install", "ZFS", "virt-clone",
		"kvm-mesh", "kldload-ca", "kldload-db", "klab"}, 10},
}

// SysTierLabel is the display form of a KldloadTier key.
func SysTierLabel(key string) string {
	for _, t := range sysTierReqs {
		if t.key == key {
			return t.label
		}
	}
	return key
}

// RunSysdiag probes the host and assembles the report. rows is the estate,
// for the VM parent dataset; nil means "work it out" (the CLI path).
// Every probe is tolerant: a failure is a red card with its reason, never
// an error out of here — the screen must open on the box that needs it most.
func RunSysdiag(rows []Row) Sysdiag {
	d := Sysdiag{Tier: KldloadTier(), Summary: Tier(), Features: sysFeatures}
	d.Host, _ = os.Hostname()

	// ── probes ──
	add := func(name string, ok bool, detail string) {
		d.Probes = append(d.Probes, SysProbe{Name: name, Detail: detail, OK: ok})
	}
	if target.SSHHost != "" {
		add("KVM", true, "remote target "+target.Host+" — the probes below describe THIS box")
	} else if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		_ = f.Close()
		add("KVM", true, "/dev/kvm readable — hardware virtualisation")
	} else {
		add("KVM", false, err.Error())
	}
	if lv, err := ConnectSystem(); err == nil {
		n := 0
		if doms, err := lv.Estate(); err == nil {
			n = len(doms)
		}
		lv.Close()
		add("libvirt", true, fmt.Sprintf("%s — %d domains", target.LibvirtURI, n))
	} else {
		add("libvirt", false, err.Error())
	}
	tool := func(name, why string) {
		if haveHostCmd(name) {
			add(name, true, why)
		} else {
			add(name, false, "not found — "+why)
		}
	}
	tool("virt-install", "builds new VMs")
	parent := ""
	if HasZFS() {
		if rows == nil {
			parent = zfsParentForBuild()
		} else {
			parent = ZFSVMParent(rows)
		}
		if parent == "" {
			add("ZFS", true, "zfs present — no VM parent dataset yet, the first zvol build makes one")
		} else {
			add("ZFS", true, "zvol-backed VMs under "+parent)
		}
	} else {
		add("ZFS", false, "no zfs on PATH — qcow2 disks, no instant clones")
	}
	tool("virt-clone", "clones a domain onto a zvol")
	tool("kvm-mesh", "per-appliance WireGuard mesh")
	tool("kldload-ca", "estate TLS leaf per VM")
	if db := kldloadDB(); db != "" {
		add("kldload-db", true, db+" — the Ansible inventory reads it")
	} else {
		add("kldload-db", false, "not found — no estate inventory")
	}
	tool("klab", "golden image lineages")
	tool("wg", "WireGuard on the host — the mesh's other half")
	met := map[string]bool{}
	for _, p := range d.Probes {
		met[p.Name] = p.OK
	}

	// ── facts ──
	fact := func(k, v string) {
		if v != "" {
			d.Facts = append(d.Facts, SysFact{k, v})
		}
	}
	fact("host", d.Host)
	fact("os", osReleaseField("PRETTY_NAME"))
	fact("kernel", readFirstLine("/proc/sys/kernel/osrelease"))
	fact("cpu", cpuSummary())
	fact("memory", memSummary())
	fact("libvirt", firstLine(3*time.Second, "virsh", "--version"))
	for _, q := range []string{"qemu-system-x86_64", "qemu-kvm", "/usr/libexec/qemu-kvm"} {
		if v := firstLine(3*time.Second, q, "--version"); v != "" {
			fact("qemu", strings.TrimPrefix(v, "QEMU emulator version "))
			break
		}
	}
	if HasZFS() {
		// through zfsRun, not a bare exec: on a remote target the pool and
		// its version live on the hypervisor, and this line must say so.
		if v, err := zfsRun("version"); err == nil {
			fact("zfs", strings.TrimSpace(strings.SplitN(v, "\n", 2)[0]))
		}
		if parent != "" {
			pool := strings.SplitN(parent, "/", 2)[0]
			if avail, err := zfsRun("list", "-H", "-o", "avail", pool); err == nil {
				fact("pool", pool+" — "+strings.TrimSpace(avail)+" free")
			}
		}
	}
	if v := firstLine(3*time.Second, "wg", "--version"); v != "" {
		fact("wireguard", strings.SplitN(v, " - ", 2)[0])
	}
	if ed := readFirstLine("/etc/kldload/edition"); ed != "" {
		fact("kldload", ed+" edition, "+readFirstLine("/etc/kldload/profile")+" profile")
	}

	// ── tiers ──
	for _, t := range sysTierReqs {
		st := SysTier{Key: t.key, Label: t.label, Has: sysTierHas(t.features)}
		for _, r := range t.reqs {
			st.Reqs = append(st.Reqs, SysReq{Name: r, Met: met[r]})
		}
		d.Tiers = append(d.Tiers, st)
	}
	return d
}

// firstLine runs argv with a deadline and returns its first stdout line, or
// "" — a version probe that hangs (a wedged libvirt socket) must not hang
// the screen that is supposed to explain the host.
func firstLine(timeout time.Duration, argv ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func readFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

// osReleaseField reads one key of /etc/os-release, unquoted.
func osReleaseField(key string) string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// cpuSummary is "model · N threads · vmx|svm" from /proc/cpuinfo, the flag
// being the one fact that decides whether KVM is hardware or a 40x-slower
// emulation.
func cpuSummary() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	model, threads, flag := "", 0, "no vmx/svm flag"
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "processor"):
			threads++
		case model == "" && strings.HasPrefix(l, "model name"):
			if _, v, ok := strings.Cut(l, ":"); ok {
				model = strings.TrimSpace(v)
			}
		case strings.HasPrefix(l, "flags") && flag == "no vmx/svm flag":
			for _, f := range strings.Fields(l) {
				if f == "vmx" || f == "svm" {
					flag = f
				}
			}
		}
	}
	if model == "" {
		return ""
	}
	return fmt.Sprintf("%s · %d threads · %s", model, threads, flag)
}

func memSummary() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "MemTotal:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				if kb, err := strconv.ParseUint(f[0], 10, 64); err == nil {
					return fmt.Sprintf("%.1f GiB", float64(kb)/1048576)
				}
			}
		}
	}
	return ""
}

// PrintSysdiag is the terminal rendering: the same report, one screen.
func PrintSysdiag(w io.Writer, d Sysdiag) {
	fmt.Fprintf(w, "sysdiag — %s\n", d.Host)
	fmt.Fprintf(w, "tier: %s — %s\n\n", SysTierLabel(d.Tier), d.Summary)
	for _, f := range d.Facts {
		fmt.Fprintf(w, "  %-10s %s\n", f.Key, f.Value)
	}
	fmt.Fprintln(w, "\nprobes")
	for _, p := range d.Probes {
		mark := "✓"
		if !p.OK {
			mark = "✗"
		}
		fmt.Fprintf(w, "  %s %-13s %s\n", mark, p.Name, p.Detail)
	}
	fmt.Fprintln(w, "\nrequirements")
	for _, t := range d.Tiers {
		var parts []string
		for _, r := range t.Reqs {
			mark := "✓"
			if !r.Met {
				mark = "✗"
			}
			parts = append(parts, mark+r.Name)
		}
		here := ""
		if t.Key == d.Tier {
			here = "   ▲ this host"
		}
		fmt.Fprintf(w, "  %-11s %s%s\n", t.Label, strings.Join(parts, "  "), here)
	}
	fmt.Fprintf(w, "\n  %-30s", "capabilities")
	for _, t := range d.Tiers {
		fmt.Fprintf(w, "%-12s", t.Label)
	}
	fmt.Fprintln(w)
	for i, f := range d.Features {
		fmt.Fprintf(w, "  %-30s", f)
		for _, t := range d.Tiers {
			mark := "—"
			if t.Has[i] {
				mark = "✓"
			}
			fmt.Fprintf(w, "%-12s", "    "+mark)
		}
		fmt.Fprintln(w)
	}
}
