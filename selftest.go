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
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
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
//
// Every mutation here goes through sudoMutate so it lands in the audit log:
// a destroy-all of twelve VMs used to leave no trace at all, and the one
// signal the operator had that it was running was the VM count going down
// (onyx, 2026-09-04). The zvol destroy retries "dataset is busy" the way
// runPlan does — qemu lets go of the disk a moment after virsh destroy,
// and the bare call here was silently leaving zvols behind.
func destroyApplianceVM(vm string) {
	sudoMutate("virsh", "destroy", vm)
	sudoMutate("virsh", "undefine", vm, "--nvram")
	// zfsArgv, not bare zfs: the routing gate exists so every dataset
	// operation lands on whichever host the session targets, and a teardown
	// that quietly ran on the wrong machine is exactly the kind of thing it
	// was written to prevent.
	if out, err := sudoRun(zfsArgv("list", "-H", "-o", "name")...); err == nil {
		for _, ds := range strings.Split(out, "\n") {
			ds = strings.TrimSpace(ds)
			if strings.HasSuffix(ds, "/"+vm) || strings.HasSuffix(ds, "/"+vm+"-data") {
				argv := zfsArgv("destroy", "-r", ds)
				out, err := sudoMutate(argv...)
				for attempt := 1; err != nil && attempt <= busyDestroyRetries &&
					busyDestroy(argv, out); attempt++ {
					time.Sleep(busyDestroyDelay(attempt))
					out, err = sudoMutate(argv...)
				}
			}
		}
	}
	for _, f := range []string{vm + ".qcow2", vm + "-data.qcow2", vm + "-seed.iso"} {
		sudoMutate("rm", "-f", "/var/lib/libvirt/images/"+f)
	}
	if haveHostCmd("kvm-mesh") {
		sudoMutate("kvm-mesh", "down", enrollMeshName(vm))
	}
	// the state-DB row retires itself: sync's pass 4 drops rows whose
	// domain no longer exists
	if haveHostCmd("kldload-networks") {
		sudoMutate("kldload-networks", "sync")
	}
}

// sudoMutate is sudoRun for a command that changes the host: the same call,
// plus the audit line every mutation owes. Read-only probes keep sudoRun,
// so the log stays a record of what was done rather than of what was asked.
func sudoMutate(args ...string) (string, error) {
	out, err := sudoRun(args...)
	rc := 0
	if err != nil {
		rc = 1
	}
	auditLog("sudo -n "+strings.Join(args, " "), rc)
	return out, err
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
		_, buildErr = waitAppliance(context.Background(), vm, a.Port, a.ProbeTCP, blogLine)
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

// buildJobs is how many tiles build-all runs at once: one, unless
// VMX_BUILD_JOBS says otherwise.
//
// HISTORY: for most of 2026-09-04 this derived a count from MemAvailable —
// "as many as fit" — and the arithmetic launched five on onyx (32GB, a
// GNOME desktop, ARC already at its floor). The five ballooned to their
// full RAM together, the kernel OOM-killed pipewire, the terminals, the
// portals and finally this program at seven of twelve tiles, and the tiles
// that finished under contention took eleven and twelve minutes each
// against under two for the first one to run alone. Parallel builds do not
// contend for disk or network; they contend for RAM, and a host that is
// swapping finishes later than one building in series. So the default is
// one at a time, with the whole host's cores and spare RAM handed to that
// one build (buildSize), which is where the wall-clock actually goes: a
// zfs dkms build on two vCPUs is five to eight minutes on its own.
//
// VMX_BUILD_JOBS=N is the operator saying "I know this host" and is taken
// as given, capped only at the tile count. Anything unparseable or under 1
// is one. Returns a count in [1, n].
func buildJobs(n int, env string) int {
	if n < 1 {
		return 1
	}
	j, err := strconv.Atoi(env)
	if err != nil || j < 1 {
		return 1
	}
	if j > n {
		return n
	}
	return j
}

// buildSize is the vCPU count and RAM a tile is BUILT with, as opposed to
// the catalog size it runs at once it is up. A first boot is package
// installs, a recipe, and for the ZFS tiles a dkms compile: all of it
// scales with cores and page cache, and none of it is what the tile needs
// afterwards. So a serial build gets most of the host — the cores less two
// for the desktop, capped at 8 where a dkms -j stops gaining, and four
// times the catalog RAM capped at 8G and at what the host can spare — and
// shrinkToCatalog hands it back the moment the tile answers.
//
// jobs != 1 returns the catalog size unchanged: parallel tiles share the
// host and the boost is what made five of them thrash. hostCPUs is the
// building host's core count, availMB its MemAvailable at the moment the
// tile starts (read per tile, since the previous one just gave its RAM
// back). Never below the catalog size, so a small host still builds.
func buildSize(a Appliance, jobs, hostCPUs, availMB int) (vcpus, ramMB int) {
	vcpus, ramMB = a.VCPUs, a.RAMMB
	if jobs != 1 {
		return vcpus, ramMB
	}
	const (
		vcpuCap    = 8
		ramCapMB   = 8192
		headroomMB = 4096 // the host, libvirt, the ARC's working set
	)
	if c := hostCPUs - 2; c > vcpus {
		vcpus = c
	}
	if vcpus > vcpuCap {
		vcpus = vcpuCap
	}
	want := 4 * a.RAMMB
	if want > ramCapMB {
		want = ramCapMB
	}
	if room := availMB - headroomMB; want > room {
		want = room
	}
	if want > ramMB {
		ramMB = want
	}
	return vcpus, ramMB
}

// shrinkToCatalog returns a tile built at buildSize to its catalog vCPU
// and RAM: memory live through the balloon and in the definition, so the
// next tile starts with the RAM the last one borrowed; vCPUs in the
// definition, and live where the guest lets them go. A refusal is logged,
// not hidden — the operator who reads "22 vCPUs" in the estate a week
// later must be able to find why here.
func shrinkToCatalog(vm string, a Appliance, builtCPUs, builtMB int, log func(string)) {
	if builtCPUs == a.VCPUs && builtMB == a.RAMMB {
		return
	}
	mem := fmt.Sprintf("%dM", a.RAMMB)
	cpus := fmt.Sprint(a.VCPUs)
	fail := func(step, out string) {
		log("  size: " + step + " failed — " + strings.SplitN(out, "\n", 2)[0])
	}
	if out, err := sudoRun("virsh", "setmem", vm, mem, "--live", "--config"); err != nil {
		fail("setmem", out)
	}
	if out, err := sudoRun("virsh", "setmaxmem", vm, mem, "--config"); err != nil {
		fail("setmaxmem", out)
	}
	// current before maximum: the maximum cannot drop below the current count
	if out, err := sudoRun("virsh", "setvcpus", vm, cpus, "--config"); err != nil {
		fail("setvcpus --config", out)
	}
	if out, err := sudoRun("virsh", "setvcpus", vm, cpus, "--config", "--maximum"); err != nil {
		fail("setvcpus --maximum", out)
	}
	if out, err := sudoRun("virsh", "setvcpus", vm, cpus, "--live"); err != nil {
		// hot-unplug needs the guest's cooperation; idle vCPUs cost the
		// host nothing, so this is a note, and the definition fixes it at
		// the next boot
		log(fmt.Sprintf("  size: %d vCPUs stay until the next reboot (live unplug refused: %s)",
			builtCPUs, strings.SplitN(out, "\n", 2)[0]))
	}
	log(fmt.Sprintf("  size: trimmed to catalog %d vCPU / %d MB", a.VCPUs, a.RAMMB))
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
// so it doubles as "build whatever is missing". Tiles build one at a time
// (buildJobs), each with most of the host's cores and spare RAM for the
// duration of its first boot (buildSize) and trimmed back to catalog size
// once it answers; every log line is prefixed with its VM so a parallel run
// under VMX_BUILD_JOBS still reads. Returns built and failed counts.
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
//
// urls is one landing URL per tile that came up AND was left running
// (VMX_BUILD_KEEP_RUNNING=1) — the GUI opens each in a browser tab when the
// run ends, so "one button" ends with the login pages in front of the
// operator rather than a list to copy out of a log (operator, 2026-09-04).
// By default every finished tile is shut off (powerOff) and urls is empty;
// the report still carries the addresses. The CLI prints the report and
// opens nothing: a terminal may be an ssh session.
//
// ctx cancels the run: tiles not yet started are not started, and the tile
// in flight is removed so the next run rebuilds it rather than skipping a
// half-configured VM as "already exists". The GUI's Cancel button and the
// CLI's Ctrl-C are the two callers; before this there was no way to stop a
// twelve-tile run short of killing the program ("there's no cancel button",
// operator, 2026-09-04).
func BuildAllAppliances(ctx context.Context, only string, log func(string)) (built, failed int, access, urls []string) {
	want := map[string]bool{}
	for _, o := range strings.Split(only, ",") {
		if o = strings.TrimSpace(strings.ToLower(o)); o != "" {
			want[o] = true
		}
	}
	// Say something before doing anything. The first line used to come
	// after twelve `sudo virsh domstate` calls, one per tile, and for those
	// seconds the window showed the intro and a greyed button: "I press
	// Build all and nothing happens" (operator, 2026-09-04). One
	// `virsh list --all --name` answers for every tile at once.
	log("build-all: started — checking which tiles already exist")
	have := domainNames()
	var todo []Appliance
	for _, a := range Appliances() {
		vm := applianceVMName(a.Name)
		if len(want) > 0 && !want[strings.ToLower(a.Name)] && !want[vm] {
			continue
		}
		if have[vm] {
			log(vm + " already exists — skipped")
			continue
		}
		todo = append(todo, a)
	}
	if len(todo) == 0 {
		if len(want) > 0 {
			log("build-all: no catalog tile matches --only " + only)
		} else {
			log("build-all: nothing to build")
		}
		return 0, 0, nil, nil
	}
	jobs := buildJobs(len(todo), os.Getenv("VMX_BUILD_JOBS"))
	head := fmt.Sprintf("build-all: %d tile(s), one at a time, each built with the host's spare cores and RAM", len(todo))
	if jobs > 1 {
		head = fmt.Sprintf("build-all: %d tile(s), %d at a time at catalog size (VMX_BUILD_JOBS)", len(todo), jobs)
	}
	log(head)
	auditLog(head, 0) // a build-all starting is a fact the audit log should carry

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)
	blocks := make([][]string, len(todo)) // catalog order, not finish order
	landing := make([]string, len(todo))
	cancelled := 0
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
			if ctx.Err() != nil {
				mu.Lock()
				cancelled++
				mu.Unlock()
				return
			}
			res, lines, url := buildOneAppliance(ctx, a, vm, jobs, tlog)
			mu.Lock()
			switch res {
			case tileBuilt:
				built++
				blocks[i] = lines
				landing[i] = url
			case tileFailed:
				failed++
			case tileCancelled:
				cancelled++
			}
			mu.Unlock()
		}(i, a)
	}
	wg.Wait()
	log("")
	sum := fmt.Sprintf("build-all: %d built, %d failed", built, failed)
	if cancelled > 0 {
		sum += fmt.Sprintf(", %d cancelled", cancelled)
	}
	log(sum)
	for i, b := range blocks {
		if len(b) > 0 {
			access = append(access, b...)
			access = append(access, "")
			if landing[i] != "" {
				urls = append(urls, landing[i])
			}
		}
	}
	return built, failed, access, urls
}

// landingURL is the one address to open for a tile that is up: the first
// URL in its LandsOn line with the address filled in, else the probe URL.
func landingURL(a Appliance, probe string) string {
	if strings.Contains(a.LandsOn, "<vm-ip>") {
		if m := landingURLRe.FindString(strings.ReplaceAll(a.LandsOn, "<vm-ip>", ipOf(probe))); m != "" {
			return m
		}
	}
	return probe
}

var landingURLRe = regexp.MustCompile(`(?:https?|rdp)://[^\s()·]+`)

// tileResult is what one tile of build-all came to.
type tileResult int

const (
	tileBuilt     tileResult = iota
	tileFailed               // logged why
	tileCancelled            // the operator stopped the run; the VM is gone
)

// buildOneAppliance is one tile of build-all: spec, build at buildSize, USB,
// wait for the port, enroll, trim to catalog size, power off. Returns the
// result, having logged any failure, and on success the tile's access block
// for the closing report. jobs is what BuildAllAppliances runs; the boost is
// only for a serial run and only on the local host, where the core count
// and MemAvailable read here are the building host's.
//
// A cancel that lands during the wait removes the VM: BuildNewVM has
// finished by then, so the domain exists, and a later run would skip it as
// built while its first boot never completed.
func buildOneAppliance(ctx context.Context, a Appliance, vm string, jobs int, log func(string)) (tileResult, []string, string) {
	log("building " + a.Name)
	// Resolved here, not inside Spec, so the generated secrets are known to
	// this side and can be reported at the end; Render resolves again and
	// keeps every value it is given.
	vals, err := a.resolve(a.Defaults())
	if err != nil {
		log("  spec: " + err.Error())
		return tileFailed, nil, ""
	}
	spec, err := a.Spec(vm, "admin", "", "", vals)
	if err != nil {
		log("  spec: " + err.Error())
		return tileFailed, nil, ""
	}
	builtCPUs, builtMB := a.VCPUs, a.RAMMB
	if target.SSHHost == "" {
		builtCPUs, builtMB = buildSize(a, jobs, runtime.NumCPU(), memAvailableMB())
	}
	if builtCPUs != a.VCPUs || builtMB != a.RAMMB {
		spec.VCPUs, spec.RAMMB = builtCPUs, builtMB
		log(fmt.Sprintf("  building with %d vCPU / %d MB (catalog %d / %d, restored once it answers)",
			builtCPUs, builtMB, a.VCPUs, a.RAMMB))
	}
	if KldloadTier() == "kldload" {
		if k := hostOpsPubkey(); k != "" {
			spec.RootSSHKeys = append(spec.RootSSHKeys, k)
		}
	}
	parent := zfsParentForBuild()
	if err := BuildNewVM(spec, parent, log); err != nil {
		log("  build FAILED: " + err.Error())
		return tileFailed, nil, ""
	}
	AttachUSBDevices(vm, a.USB, log)
	url, err := waitAppliance(ctx, vm, a.Port, a.ProbeTCP, log)
	if errors.Is(err, context.Canceled) {
		log("  cancelled — removing " + vm + " so the next run rebuilds it")
		destroyApplianceVM(vm)
		return tileCancelled, nil, ""
	}
	if err != nil {
		log("  came up but did not answer: " + err.Error())
		return tileFailed, nil, ""
	}
	EnrollAppliance(vm, applianceSlug(a.Name), log)
	shrinkToCatalog(vm, a, builtCPUs, builtMB, log)
	if a.ProbeTCP {
		url = "rdp://" + url // the one TCP tile today; the scheme is what a client wants
	}
	log("  " + a.Name + " ready on " + url)
	access := applianceAccess(a, spec, vals, url)
	if keepRunning() {
		return tileBuilt, access, landingURL(a, url)
	}
	powerOff(vm, log)
	access = append(access, "  state: shut off — start it from the estate when wanted")
	return tileBuilt, access, "" // nothing to open: the page is not being served
}

// keepRunning is VMX_BUILD_KEEP_RUNNING=1: leave every built tile up and
// open its page at the end. The default is off — see powerOff.
func keepRunning() bool {
	return os.Getenv("VMX_BUILD_KEEP_RUNNING") == "1"
}

// powerOff shuts a finished tile down and waits for it. A build-all is
// "one of everything, kept" — kept as a VM to start when wanted, not as
// twelve services running at once: after the run that built the web stack
// the operator's next words were that finished tiles should be shut off
// (onyx, 2026-09-04), and twelve idle appliances were most of what put a
// 32G host into the OOM killer earlier that day. The report still carries
// every URL and login; the estate starts the VM. Graceful first, and a
// guest that ignores the ACPI button for two minutes is forced — a fresh
// cloud image with nothing but a just-configured service loses nothing to
// that, and a tile left running against the operator's ask is the worse
// outcome.
func powerOff(vm string, log func(string)) {
	state := func() string {
		out, _ := sudoRun("virsh", "domstate", vm)
		return strings.TrimSpace(out)
	}
	if out, err := sudoMutate("virsh", "shutdown", vm); err != nil {
		log("  shutdown: " + strings.SplitN(out, "\n", 2)[0])
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for state() != "shut off" {
		if time.Now().After(deadline) {
			log("  did not shut down in 2 minutes — forcing it off")
			if out, err := sudoMutate("virsh", "destroy", vm); err != nil {
				log("  destroy: " + strings.SplitN(out, "\n", 2)[0])
				return
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	log("  shut off — start it from the estate when wanted")
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
	// Name, then what it is in one line, then where it is: a report read a
	// week later has to say what "app-sdr-statio" was for.
	out := []string{fmt.Sprintf("%s  (%s)", a.Name, spec.Name), "  " + a.Summary, "  " + where}
	// The upstream project's page beside the instance that was just built:
	// the report is where the operator goes to find their new service, and
	// its docs are the next thing they want ("the home page is nice too",
	// operator, 2026-09-04). A link in the report, not a tab — the browser
	// gets the instances, twelve more tabs of documentation would be noise.
	if a.Homepage != "" {
		out = append(out, "  docs: "+a.Homepage)
	}
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
// domainNames is every domain libvirt knows, running or not, in one call —
// what a loop over the catalog asks instead of twelve domainExists.
func domainNames() map[string]bool {
	out, err := sudoRun("virsh", "list", "--all", "--name")
	have := map[string]bool{}
	if err != nil {
		return have
	}
	for _, n := range strings.Fields(out) {
		have[n] = true
	}
	return have
}

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
