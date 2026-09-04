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
	"fmt"
	"os"
	"strings"
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
		_, buildErr = WaitAppliance(vm, a.Port, blogLine)
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
