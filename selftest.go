// selftest.go — the build-and-audit-everything button.
//
// WHAT IT DOES, per catalog tile, sequentially:
//  1. Builds it exactly the way an operator does — Spec with defaults and
//     generated secrets, BuildNewVM, USB attach, WaitAppliance, enrollment.
//  2. Audits OUTCOMES, counted against what was attempted:
//     the recipe's own in-guest verdict (RESULT: VERIFIED, N/M checks),
//     the pool on the data disk for NeedsZFS tiles, the service answering
//     on its port inside the guest, a LIVE management-mesh handshake, the
//     estate cert, the state-DB row.
//  3. Tears the VM down on a pass; KEEPS it (plus the build log under
//     /tmp/selftest-<vm>.log) on a failure, because a destroyed failure
//     cannot be diagnosed.
//
// WHY IN GO, IN THE BINARY: the first version was a shell script beside the
// binary, which is the two-divergent-copies trap this project already paid
// for once with wgx — the tool that owns the catalog must own its proof.
// contrib/smoke-appliances.sh survives only as a one-line wrapper.
//
// Every fault this engine's shell ancestor found in its first three runs —
// valkey's virtual provide, the absent in-guest ZFS, the fc43 gpg key name,
// pg_hba ordering, SELinux label inheritance, unexported fields, missing wg
// in guests — was invisible to every static gate. Deploying for real is the
// only audit that counts.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type selfTestResult struct {
	App    string
	VM     string
	Pass   int
	Fail   int
	Detail []string
	Kept   bool
}

// selfTestVM derives the per-tile VM name: short (the mesh iface prepends
// "ap-" and the kernel caps interfaces at 15), deterministic, and clearly
// disposable in an estate listing.
func selfTestVM(appName string) string {
	s := applianceSlug(appName)
	if len(s) > 9 {
		s = s[:9]
	}
	return strings.TrimRight("st-"+s, "-")
}

// destroyApplianceVM unwinds everything a tile build creates, newest-first,
// every step tolerant of absence — teardown is how the NEXT run gets a clean
// field, so it must work from any partial state.
func destroyApplianceVM(vm string) {
	_, _ = sudoRun("virsh", "destroy", vm)
	_, _ = sudoRun("virsh", "undefine", vm, "--nvram")
	// zfsArgv, not bare zfs: the routing gate exists so every dataset
	// operation lands on whichever host the session targets, and a teardown
	// that quietly ran on the wrong machine is exactly the kind of thing it
	// was written to prevent.
	if out, err := sudoRun(zfsArgv("list", "-H", "-o", "name")...); err == nil {
		for _, ds := range strings.Split(out, "\n") {
			ds = strings.TrimSpace(ds)
			if strings.HasSuffix(ds, "/"+vm) || strings.HasSuffix(ds, "/"+vm+"-data") {
				_, _ = sudoRun(zfsArgv("destroy", "-r", ds)...)
			}
		}
	}
	for _, f := range []string{vm + ".qcow2", vm + "-data.qcow2", vm + "-seed.iso"} {
		_, _ = sudoRun("rm", "-f", "/var/lib/libvirt/images/"+f)
	}
	if haveHostCmd("kvm-mesh") {
		_, _ = sudoRun("kvm-mesh", "down", enrollMeshName(vm))
	}
	// the state-DB row retires itself: sync's pass 4 drops rows whose
	// domain no longer exists
	if haveHostCmd("kldload-networks") {
		_, _ = sudoRun("kldload-networks", "sync")
	}
}

// selfTestOne builds and audits a single tile. Returns the result; the VM is
// torn down only when everything passed and keep is false.
func selfTestOne(a Appliance, keep bool, log func(string)) selfTestResult {
	vm := selfTestVM(a.Name)
	r := selfTestResult{App: a.Name, VM: vm}
	check := func(label string, ok bool) {
		mark := "OK"
		if !ok {
			mark = "FAIL"
			r.Fail++
		} else {
			r.Pass++
		}
		line := fmt.Sprintf("  %-38s %s", label, mark)
		r.Detail = append(r.Detail, line)
		log(line)
	}

	log("")
	log("══ " + a.Name + " → " + vm + " ══")
	destroyApplianceVM(vm) // a stale twin poisons every check below

	spec, err := a.Spec(vm, "admin", "", "", a.Defaults())
	if err != nil {
		check("spec renders", false)
		log("    " + err.Error())
		return r
	}
	if KldloadTier() == "kldload" {
		if k := hostOpsPubkey(); k != "" {
			spec.RootSSHKeys = append(spec.RootSSHKeys, k)
		}
	}

	parent := ""
	if lv, err := ConnectSystem(); err == nil {
		if doms, err := lv.Estate(); err == nil && HasZFS() {
			dss, _ := ListDatasets()
			snaps, _ := ListSnapshots()
			rs, _ := LoadRules("")
			var rows []Row
			for _, g := range BuildEstate(doms, dss, snaps, rs, LoadAnnotations()) {
				rows = append(rows, g.Rows...)
			}
			parent = ZFSVMParent(rows)
		}
		lv.Close()
	}

	blog, _ := os.Create("/tmp/selftest-" + vm + ".log")
	blogLine := func(l string) {
		if blog != nil {
			fmt.Fprintln(blog, l)
		}
	}
	buildErr := BuildNewVM(spec, parent, blogLine)
	if buildErr == nil {
		AttachUSBDevices(vm, a.USB, blogLine)
		_, buildErr = waitAppliance(vm, a.Port, a.ProbeTCP, blogLine)
	}
	if buildErr == nil {
		EnrollAppliance(vm, applianceSlug(a.Name), blogLine)
	}
	if blog != nil {
		blog.Close()
	}
	check("build + port wait", buildErr == nil)
	if buildErr != nil {
		log("    " + buildErr.Error())
	}

	ip := ""
	if lv, err := ConnectSystem(); err == nil {
		if ips, err := lv.LeaseIPs(vm); err == nil && len(ips) > 0 {
			ip = ips[0]
		}
		lv.Close()
	}
	if ip == "" {
		check("guest address", false)
		r.Kept = true
		log("  kept for diagnosis: /tmp/selftest-" + vm + ".log")
		return r
	}
	log("  guest: " + ip + "   (build log: /tmp/selftest-" + vm + ".log)")

	_, sshErr := enrollGuestSSH(ip, "true")
	check("root ssh (seeded ops key)", sshErr == nil)

	// The recipe's own verdict. Substrate recipes end in app_summary; the
	// pre-substrate ones do not, and their absence is reported as informative
	// rather than failed — a check that cannot exist is not a check that
	// failed.
	if sshErr == nil && strings.Contains(a.Script, "app_summary") {
		verdict := ""
		for i := 0; i < 6 && verdict == ""; i++ {
			out, _ := enrollGuestSSH(ip,
				"grep -hoE 'RESULT: (VERIFIED|INCOMPLETE)' /var/log/cloud-init-output.log 2>/dev/null | tail -1")
			verdict = strings.TrimSpace(out)
			if verdict == "" {
				time.Sleep(10 * time.Second)
			}
		}
		check("recipe verdict VERIFIED", verdict == "RESULT: VERIFIED")
		if n, _ := enrollGuestSSH(ip,
			"grep -hoE '[0-9]+/[0-9]+ checks passed' /var/log/cloud-init-output.log 2>/dev/null | tail -1"); strings.TrimSpace(n) != "" {
			log("    guest checks: " + strings.TrimSpace(n))
		}
		if verdict == "RESULT: INCOMPLETE" {
			if fails, _ := enrollGuestSSH(ip,
				"grep -E ' FAIL$' /var/log/cloud-init-output.log | tail -5"); fails != "" {
				for _, l := range strings.Split(fails, "\n") {
					log("    " + l)
				}
			}
		}
	}

	if sshErr == nil && a.Needs == NeedsZFS {
		_, e := enrollGuestSSH(ip, "zpool list -H tank")
		check("in-guest pool on the data disk", e == nil)
	}
	if sshErr == nil {
		_, e := enrollGuestSSH(ip, fmt.Sprintf(
			"curl -fsSL --max-time 8 http://127.0.0.1:%d/ >/dev/null", a.Port))
		check(fmt.Sprintf("service answers on :%d", a.Port), e == nil)
	}

	if KldloadTier() == "kldload" {
		mesh := enrollMeshName(vm)
		out, _ := sudoRun("wg", "show", mesh, "latest-handshakes")
		live := false
		now := time.Now().Unix()
		for _, l := range strings.Split(out, "\n") {
			f := strings.Fields(l)
			if len(f) == 2 {
				var ts int64
				fmt.Sscanf(f[1], "%d", &ts)
				if ts > 0 && now-ts < 180 {
					live = true
				}
			}
		}
		check("mesh "+mesh+" live handshake", live)
		if sshErr == nil {
			_, e := enrollGuestSSH(ip, "test -f /etc/kldload/tls/server.crt")
			check("estate cert staged in guest", e == nil)
		}
		if haveHostCmd("kldload-db") {
			out, _ := sudoRun("kldload-db", "dump")
			check("registered in the state DB", strings.Contains(out, "\""+vm+"\""))
		}
	}

	log(fmt.Sprintf("  ── %d passed, %d failed", r.Pass, r.Fail))
	if r.Fail == 0 && !keep {
		destroyApplianceVM(vm)
		log("  (torn down)")
	} else if r.Fail > 0 {
		r.Kept = true
		log("  kept for diagnosis: virsh console " + vm + " · /tmp/selftest-" + vm + ".log")
	}
	return r
}

// SelfTestAppliances runs the whole catalog (or one tile). Returns the number
// of failed tiles — the process exit status.
func SelfTestAppliances(only string, keep bool, log func(string)) int {
	apps := Appliances()
	var results []selfTestResult
	failed := 0
	for _, a := range apps {
		if only != "" && !strings.EqualFold(a.Name, only) &&
			selfTestVM(a.Name) != only {
			continue
		}
		r := selfTestOne(a, keep, log)
		results = append(results, r)
		if r.Fail > 0 {
			failed++
		}
	}
	log("")
	log("══════════ appliance self-test summary ══════════")
	for _, r := range results {
		verdict := "PASS"
		if r.Fail > 0 {
			verdict = fmt.Sprintf("FAIL (%d/%d checks)", r.Fail, r.Pass+r.Fail)
		}
		log(fmt.Sprintf("  %-24s %-10s %s", r.App, r.VM, verdict))
	}
	log(fmt.Sprintf("  %d tile(s), %d failed", len(results), failed))
	return failed
}

// applianceVMName is the persistent-build name for a tile: the same short,
// deterministic slug the self-test uses, so "build all" and a later
// "destroy all" agree on what exists. "app-" not "st-": these are meant to
// stay, and the estate list should not read them as test scaffolding.
func applianceVMName(appName string) string {
	s := applianceSlug(appName)
	if len(s) > 10 {
		s = s[:10]
	}
	return strings.TrimRight("app-"+s, "-")
}

// buildJobs is how many tiles build-all runs at once: every tile in
// parallel when the host has the memory for it, fewer when it does not.
//
// WHY parallel at all: ten tiles in series was an afternoon — each one is a
// cloud-image convert, a first boot, and for the ZFS tiles a 5-8 minute
// dkms build on two vCPUs, while the other 22 cores sat idle (onyx,
// 2026-09-04, "can the build all button build everything in parallel?").
// The guests do not contend for anything but RAM: each one gets its own
// zvol, seed ISO, transient domain and lease.
//
// WHY bounded by memory and not cores: a guest that cannot get its RAM is
// swapped by the host, and ten guests swapping together finish later than
// ten in series. The bound is MemAvailable minus headroom for the host,
// divided by the largest tile's RAM, so the wave that starts is one that
// fits. VMX_BUILD_JOBS overrides it for a host the arithmetic misjudges.
//
// Known bias: on a ZFS host the ARC is not counted as reclaimable in
// MemAvailable, so after a few image converts the reading is lower than
// what the guests could actually get (onyx: 21G before a two-tile run,
// 7G during it, with the ARC at 15G). The bound therefore errs toward
// fewer tiles, never more — the right way to be wrong here.
//
// Args: n is the number of tiles to build, maxTileMB the largest RAM
// request among them, availMB the host's MemAvailable, env the value of
// VMX_BUILD_JOBS ("" when unset). Returns a count in [1, n].
func buildJobs(n, maxTileMB, availMB int, env string) int {
	if n < 1 {
		return 1
	}
	if env != "" {
		if j, err := strconv.Atoi(env); err == nil && j >= 1 {
			if j > n {
				return n
			}
			return j
		}
	}
	const headroomMB = 4096 // the host, libvirt, the ARC's working set
	if maxTileMB < 1 {
		maxTileMB = 1
	}
	j := (availMB - headroomMB) / maxTileMB
	if j < 1 {
		return 1
	}
	if j > n {
		return n
	}
	return j
}

// memAvailableMB reads MemAvailable from /proc/meminfo. 0 when it cannot,
// which buildJobs treats as "no room" and serialises — the safe reading.
func memAvailableMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == "MemAvailable:" {
			kb, _ := strconv.Atoi(fs[1])
			return kb / 1024
		}
	}
	return 0
}

// BuildAllAppliances builds every tile as a KEPT VM (no audit, no teardown) —
// the "give me one of everything" button. Already-present VMs are left alone,
// so it doubles as "build whatever is missing". Tiles build in parallel, as
// many at once as the host's memory allows (buildJobs); every log line is
// prefixed with its VM so the interleaved stream still reads. Returns built
// and failed counts.
//
// The host-side steps that two builds must not run at the same moment —
// fetching a shared cloud image, allocating a mesh subnet, issuing a cert —
// take their own locks inside BuildNewVM and EnrollAppliance; this loop does
// not need to know about them.
//
// only, when not empty, is a comma-separated list of tile names or app-
// VM names and restricts the run to those — the same addressing --selftest
// --only uses, so two tiles can be built side by side without the other
// eight.
//
// access is the closing report: one block per tile that came up, with its
// URL and every login the operator needs — the guest account, root over ssh
// where the host's ops key was seeded, and each secret the recipe was
// given or generated. It is returned rather than logged so each caller can
// put it where it belongs: the CLI on stdout, the GUI as the last thing in
// the window. Before this the generated passwords existed only inside the
// guest under /root, and a ten-tile build ended with ten VMs the operator
// had to ssh into one by one to learn how to log in (onyx, 2026-09-04).
func BuildAllAppliances(only string, log func(string)) (built, failed int, access []string) {
	want := map[string]bool{}
	for _, o := range strings.Split(only, ",") {
		if o = strings.TrimSpace(strings.ToLower(o)); o != "" {
			want[o] = true
		}
	}
	var todo []Appliance
	maxRAM := 0
	for _, a := range Appliances() {
		vm := applianceVMName(a.Name)
		if len(want) > 0 && !want[strings.ToLower(a.Name)] && !want[vm] {
			continue
		}
		if domainExists(vm) {
			log(vm + " already exists — skipped")
			continue
		}
		todo = append(todo, a)
		if a.RAMMB > maxRAM {
			maxRAM = a.RAMMB
		}
	}
	if len(todo) == 0 {
		if len(want) > 0 {
			log("build-all: no catalog tile matches --only " + only)
		} else {
			log("build-all: nothing to build")
		}
		return 0, 0, nil
	}
	jobs := buildJobs(len(todo), maxRAM, memAvailableMB(), os.Getenv("VMX_BUILD_JOBS"))
	head := fmt.Sprintf("build-all: %d tile(s), %d at a time (VMX_BUILD_JOBS overrides)", len(todo), jobs)
	log(head)
	auditLog(head, 0) // a build-all starting is a fact the audit log should carry

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)
	blocks := make([][]string, len(todo)) // catalog order, not finish order
	for i, a := range todo {
		wg.Add(1)
		go func(i int, a Appliance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			vm := applianceVMName(a.Name)
			tlog := func(l string) {
				if l == "" {
					return
				}
				log("[" + vm + "] " + l)
			}
			ok, lines := buildOneAppliance(a, vm, tlog)
			mu.Lock()
			if ok {
				built++
				blocks[i] = lines
			} else {
				failed++
			}
			mu.Unlock()
		}(i, a)
	}
	wg.Wait()
	log("")
	log(fmt.Sprintf("build-all: %d built, %d failed", built, failed))
	for _, b := range blocks {
		if len(b) > 0 {
			access = append(access, b...)
			access = append(access, "")
		}
	}
	return built, failed, access
}

// buildOneAppliance is one tile of build-all: spec, build, USB, wait for
// the port, enroll. Returns ok=false on any failure, having logged why, and
// on success the tile's access block for the closing report.
func buildOneAppliance(a Appliance, vm string, log func(string)) (bool, []string) {
	log("building " + a.Name)
	// Resolved here, not inside Spec, so the generated secrets are known to
	// this side and can be reported at the end; Render resolves again and
	// keeps every value it is given.
	vals, err := a.resolve(a.Defaults())
	if err != nil {
		log("  spec: " + err.Error())
		return false, nil
	}
	spec, err := a.Spec(vm, "admin", "", "", vals)
	if err != nil {
		log("  spec: " + err.Error())
		return false, nil
	}
	if KldloadTier() == "kldload" {
		if k := hostOpsPubkey(); k != "" {
			spec.RootSSHKeys = append(spec.RootSSHKeys, k)
		}
	}
	parent := zfsParentForBuild()
	if err := BuildNewVM(spec, parent, log); err != nil {
		log("  build FAILED: " + err.Error())
		return false, nil
	}
	AttachUSBDevices(vm, a.USB, log)
	url, err := waitAppliance(vm, a.Port, a.ProbeTCP, log)
	if err != nil {
		log("  came up but did not answer: " + err.Error())
		return false, nil
	}
	EnrollAppliance(vm, applianceSlug(a.Name), log)
	if a.ProbeTCP {
		url = "rdp://" + url // the one TCP tile today; the scheme is what a client wants
	}
	log("  " + a.Name + " ready on " + url)
	return true, applianceAccess(a, spec, vals, url)
}

// ipOf strips a probe URL (http://h:p/, rdp://h:p, h:p) down to its host.
func ipOf(url string) string {
	ip := strings.TrimPrefix(url, "http://")
	ip = strings.TrimPrefix(ip, "rdp://")
	if i := strings.IndexAny(ip, ":/"); i >= 0 {
		ip = ip[:i]
	}
	return ip
}

// applianceAccess renders one tile's block for the closing report: the
// URL it serves on, the guest login, root over ssh when the host's ops key
// was seeded, and every secret field with its value — the ones the
// recipe generated are the ones nobody has seen yet.
func applianceAccess(a Appliance, spec NewVMSpec, vals map[string]string, url string) []string {
	// The tile's own landing line, with the address filled in, when it has
	// one: the probe URL is the health port, and for the VDI tile that is
	// nginx's 404 root while the desktop is on :8889 — the operator opened
	// the reported URL and got nothing (onyx, 2026-09-04).
	where := url
	if strings.Contains(a.LandsOn, "<vm-ip>") {
		where = strings.ReplaceAll(a.LandsOn, "<vm-ip>", ipOf(url))
	}
	out := []string{fmt.Sprintf("%s  (%s)", a.Name, spec.Name), "  " + where}
	for _, h := range a.ClientHint {
		out = append(out, "  "+strings.ReplaceAll(h, "<vm-ip>", ipOf(url)))
	}
	pw := spec.Password
	if pw == "" && spec.SSHKey == "" {
		pw = DefaultGuestPassword // the same rule BuildNewVM applies
	}
	switch {
	case pw != "" && spec.SSHKey != "":
		out = append(out, fmt.Sprintf("  guest login: %s / %s  (or your ssh key)", spec.User, pw))
	case pw != "":
		out = append(out, fmt.Sprintf("  guest login: %s / %s", spec.User, pw))
	default:
		out = append(out, fmt.Sprintf("  guest login: %s with your ssh key", spec.User))
	}
	if len(spec.RootSSHKeys) > 0 {
		out = append(out, "  root: ssh root@<ip> with the host's ops key")
	}
	for _, f := range a.Fields {
		if !f.Secret && !f.Generate {
			continue
		}
		if v := vals[f.Key]; v != "" {
			out = append(out, fmt.Sprintf("  %s (%s): %s", f.Label, f.Key, v))
		}
	}
	return out
}

// ExistingApplianceVMs lists the VMs this tool built that libvirt still knows
// — the kept "app-" builds and any "st-" self-test leftover — in catalog
// order. It is what destroy-all shows before it acts and then acts on, so the
// confirmation and the deed cannot disagree.
func ExistingApplianceVMs() []string {
	var out []string
	for _, a := range Appliances() {
		for _, vm := range []string{applianceVMName(a.Name), selfTestVM(a.Name)} {
			if domainExists(vm) {
				out = append(out, vm)
			}
		}
	}
	return out
}

// DestroyAllAppliances tears down every VM this tool builds — both the kept
// "app-" builds AND the "st-" self-test leftovers, because "destroy all" that
// left half the scaffolding behind would be a lie. Estate VMs (klab-*,
// kldload-*, anything not matching our prefixes) are never touched: the list
// comes from the catalog's own names, not from a prefix scan of the estate.
func DestroyAllAppliances(log func(string)) int {
	vms := ExistingApplianceVMs()
	for _, vm := range vms {
		log("destroying " + vm)
		destroyApplianceVM(vm)
	}
	log(fmt.Sprintf("destroy-all: %d removed", len(vms)))
	return len(vms)
}

// domainExists is true when libvirt knows a domain by this name, running or
// not — the guard both build-all (skip) and destroy-all (act) turn on.
func domainExists(vm string) bool {
	out, err := sudoRun("virsh", "domstate", vm)
	return err == nil && strings.TrimSpace(out) != ""
}

// estateRows is the estate as the GUI sees it, built once on demand — for
// the paths that have no estate pane to ask: the CLI, the batch builders.
func estateRows() []Row {
	lv, err := ConnectSystem()
	if err != nil {
		return nil
	}
	defer lv.Close()
	doms, err := lv.Estate()
	if err != nil {
		return nil
	}
	var dss map[string]*Dataset
	var snaps map[string][]string
	if HasZFS() {
		dss, _ = ListDatasets()
		snaps, _ = ListSnapshots()
	}
	rs, _ := LoadRules("")
	var rows []Row
	for _, g := range BuildEstate(doms, dss, snaps, rs, LoadAnnotations()) {
		rows = append(rows, g.Rows...)
	}
	return rows
}

// zfsParentForBuild resolves the pool parent for new VM disks, or "" for the
// qcow2 fallback — the same probe the CLI and GUI build paths do, factored
// out so the batch builders share it.
func zfsParentForBuild() string {
	if !HasZFS() {
		return ""
	}
	return ZFSVMParent(estateRows())
}

// rowForDomain finds one domain's estate row — with its zvol, which is what
// MakeGolden needs — or ok=false when libvirt does not know the name.
func rowForDomain(name string) (Row, bool) {
	for _, r := range estateRows() {
		if !r.Synthetic && r.D.Name == name {
			return r, true
		}
	}
	return Row{}, false
}
