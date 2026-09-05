// firecracker.go — the estate's view of kfire microVMs.
//
// What it does, in order:
//  1. Reads `kfire list --json` and `kfire goldens --json` — the host tool
//     owns the instances, their zvols, taps, units and state.db rows; this
//     file only looks.
//  2. Turns instances into estate Rows under one group, "firecracker", so
//     the tree, the dossier, the batch bar and the TUI show them beside
//     the libvirt domains with no second list to learn.
//  3. Routes the verbs that apply — start, shut down, force off, delete,
//     and "Firecracker golden" on a shut-off appliance — to kfire as plans,
//     so they run, audit and refresh exactly like the virsh ones.
//
// WHY a separate runtime at all: the Web Stack tile is four minutes to
// build under libvirt and 250 ms to stamp from its zvols under Firecracker,
// serving HTTP seven seconds later on ~300 MB of host memory (onyx,
// 2026-09-05). The golden is built once, the slow way, with the whole
// appliance pipeline; the copies are microVMs. "It should have its own
// firecracker section in vmxplore" (operator, 2026-09-05) — this is it.
//
// Inputs: kfire on PATH (a kldload KVM host ships it), and sudo -n for the
// non-root GUI, since kfire needs root for zfs, taps and units.
// Outputs: Rows with Row.FC set; verb plans whose argv is kfire's.
//
// Notes: the estate refreshes every two seconds and a kfire call is a
// process plus a virsh lease read, so instances are cached for ten seconds
// here. A stamp or a delete calls fcInvalidate so the next refresh is
// honest rather than ten seconds stale.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// FCInstance is one row of `kfire list --json`.
type FCInstance struct {
	Name     string `json:"name"`
	Golden   string `json:"golden"`
	MAC      string `json:"mac"`
	Tap      string `json:"tap"`
	Bridge   string `json:"bridge"`
	VCPUs    int    `json:"vcpus"`
	RAMMB    int    `json:"ram_mb"`
	Port     int    `json:"port"`
	RootZvol string `json:"root_zvol"`
	DataZvol string `json:"data_zvol"`
	State    string `json:"state"` // running / shut off
	IP       string `json:"ip"`
}

// FCGolden is one row of `kfire goldens --json`.
type FCGolden struct {
	Name     string `json:"name"`
	VCPUs    int    `json:"vcpus"`
	RAMMB    int    `json:"ram_mb"`
	Port     int    `json:"port"`
	DataZvol string `json:"data_zvol"`
	Clones   int    `json:"clones"`
}

const fcGroupLabel = "firecracker"

// fcAsRoot is read once; tests pin it so the argv they assert is stable.
var fcAsRoot = os.Geteuid() == 0

// kfireArgv is the fixed argv for one kfire verb, escalated with sudo -n
// when this process is not root — the same rule the zfs mutations follow.
func kfireArgv(args ...string) []string {
	argv := append([]string{"kfire"}, args...)
	if fcAsRoot {
		return argv
	}
	return append([]string{"sudo", "-n"}, argv...)
}

func kfireAvailable() bool {
	_, err := exec.LookPath("kfire")
	return err == nil
}

func kfireJSON(v any, args ...string) error {
	argv := kfireArgv(args...)
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return json.Unmarshal(out, v)
}

func fcInstances() ([]FCInstance, error) {
	var out []FCInstance
	if err := kfireJSON(&out, "list", "--json"); err != nil {
		return nil, err
	}
	return out, nil
}

func fcGoldens() ([]FCGolden, error) {
	var out []FCGolden
	if err := kfireJSON(&out, "goldens", "--json"); err != nil {
		return nil, err
	}
	return out, nil
}

// fcRows shapes instances as estate rows. State and address come from
// kfire; memory is the ceiling (Firecracker commits lazily), which is
// what the operator sized. Persistent is true because a stopped instance
// keeps its zvols and identity — the opposite of a transient domain.
func fcRows(insts []FCInstance) []Row {
	rows := make([]Row, 0, len(insts))
	for i := range insts {
		in := insts[i]
		d := Dom{
			Name:       in.Name,
			Active:     in.State == "running",
			State:      in.State,
			VCPUs:      uint64(in.VCPUs),
			CurMemKiB:  uint64(in.RAMMB) * 1024,
			MaxMemKiB:  uint64(in.RAMMB) * 1024,
			Persistent: true,
		}
		if in.IP != "" {
			d.IPs = []string{in.IP}
		}
		rows = append(rows, Row{D: d, Backing: in.RootZvol, Group: fcGroupLabel, FC: &in})
	}
	return rows
}

// fcGroupCached is the "firecracker" group, or false when kfire is absent
// or has nothing. Ten-second cache: see the banner.
var (
	fcMu      sync.Mutex
	fcAt      time.Time
	fcCached  []FCInstance
	fcGoldenC []FCGolden
)

func fcInvalidate() {
	fcMu.Lock()
	fcAt = time.Time{}
	fcMu.Unlock()
}

// fcRefresh reads instances and goldens when the cache is older than ten
// seconds. A kfire that cannot answer (no sudo, no state dir yet) means
// empty lists, not a crash of the estate view; the audit log has the why.
func fcRefresh() {
	if time.Since(fcAt) <= 10*time.Second {
		return
	}
	insts, err := fcInstances()
	if err != nil {
		auditLog("kfire list --json: "+err.Error(), 1)
		insts = nil
	}
	gs, err := fcGoldens()
	if err != nil {
		auditLog("kfire goldens --json: "+err.Error(), 1)
		gs = nil
	}
	fcCached, fcGoldenC, fcAt = insts, gs, time.Now()
}

// fcRowsCached is every instance as an estate row — empty, not absent,
// when there are none: the Firecracker branch is a fixture of a host that
// has kfire, so the operator can find the goldens and the Stamp row
// before the first instance exists ("I still don't see it", 2026-09-05,
// looking at an estate with nothing stamped yet).
func fcRowsCached() []Row {
	if !kfireAvailable() {
		return nil
	}
	fcMu.Lock()
	defer fcMu.Unlock()
	fcRefresh()
	return fcRows(fcCached)
}

func fcGoldensCached() []FCGolden {
	if !kfireAvailable() {
		return nil
	}
	fcMu.Lock()
	defer fcMu.Unlock()
	fcRefresh()
	return fcGoldenC
}

// fcGroupCached is the instances as one estate group for the TUI's flat
// list; false when kfire is absent or nothing is stamped.
func fcGroupCached() (GroupRows, bool) {
	rows := fcRowsCached()
	if len(rows) == 0 {
		return GroupRows{}, false
	}
	return GroupRows{Label: fcGroupLabel, Rows: rows}, true
}

// withFirecracker appends the firecracker group to an estate — the one
// call the GUI and the TUI make after BuildEstate.
func withFirecracker(groups []GroupRows) []GroupRows {
	if g, ok := fcGroupCached(); ok {
		groups = append(groups, g)
	}
	return groups
}

// streamCmd runs argv and hands every output line to log as it arrives,
// which is what a stamp needs: each instance prints as it comes up, and a
// ten-instance --wait is half a minute nobody wants to stare at a blank
// window for. ctx cancels by killing the process. Audited like a plan.
func streamCmd(ctx context.Context, log func(string), argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			log(sc.Text())
		}
		close(done)
	}()
	err := cmd.Wait()
	_ = pw.Close()
	<-done
	rc := 0
	if err != nil {
		rc = 1
	}
	auditLog(strings.Join(argv, " "), rc)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ─── verb plans ─────────────────────────────────────────────────────────

// fcRefuse is every verb that has no meaning for a microVM: snapshots,
// clones and rollbacks belong to the golden it was stamped from, and
// suspend/reboot/autostart are libvirt's. Naming the verb keeps the error
// from reading as a bug.
func fcRefuse(r Row, verb string) (verbPlan, error) {
	return verbPlan{}, fmt.Errorf("%s is a Firecracker microVM — %s is not a microVM verb (kfire golden/stamp/destroy are)", r.D.Name, verb)
}

func planFCStart(r Row) (verbPlan, error) {
	if r.D.State == "running" {
		return verbPlan{}, fmt.Errorf("%s is already running", r.D.Name)
	}
	return verbPlan{
		title: "start " + r.D.Name + " (Firecracker)",
		cmds:  [][]string{kfireArgv("start", r.D.Name)},
	}, nil
}

func planFCShutdown(r Row) (verbPlan, error) {
	if r.D.State != "running" {
		return verbPlan{}, fmt.Errorf("%s is not running", r.D.Name)
	}
	return verbPlan{
		title: "shut down " + r.D.Name + " (Ctrl-Alt-Del over the Firecracker API)",
		cmds:  [][]string{kfireArgv("stop", r.D.Name)},
	}, nil
}

func planFCForceOff(r Row) (verbPlan, error) {
	if r.D.State != "running" {
		return verbPlan{}, fmt.Errorf("%s is not up", r.D.Name)
	}
	return verbPlan{
		title: "force off " + r.D.Name + " (Firecracker)",
		cmds:  [][]string{{"sudo", "-n", "systemctl", "stop", "kfire-" + r.D.Name}},
		warn:  "terminates the microVM — the guest gets no chance to flush",
	}, nil
}

func planFCDelete(r Row) (verbPlan, error) {
	warn := "removes the microVM, its zvol clones"
	if r.FC != nil && r.FC.DataZvol != "" {
		warn += " (root and data)"
	}
	warn += ", tap, seed and state.db row — kfire destroy"
	return verbPlan{
		title: "DELETE " + r.D.Name + " (Firecracker)",
		cmds:  [][]string{kfireArgv("destroy", r.D.Name)},
		warn:  warn,
	}, nil
}

// planFCGolden makes a shut-off appliance VM a Firecracker golden: kfire
// snapshots its zvols @kfire and pulls the kernel off its root. The VM
// itself is untouched and can be started again; the snapshot is what the
// stamps clone. Distinct from "Make Golden", which seals for kvm-clone.
func planFCGolden(r Row) (verbPlan, error) {
	if r.FC != nil {
		return fcRefuse(r, "golden")
	}
	if r.Synthetic || r.DS == nil {
		return verbPlan{}, fmt.Errorf("%s has no zvol behind it — a Firecracker golden needs the appliance layout (rpool/vms/<vm>)", r.D.Name)
	}
	if r.D.State != "shut off" {
		return verbPlan{}, fmt.Errorf("%s is %s — shut it down first; a golden is taken from a quiet disk", r.D.Name, r.D.State)
	}
	if !kfireAvailable() {
		return verbPlan{}, fmt.Errorf("kfire is not on this host — it ships with kldload's KVM host")
	}
	return verbPlan{
		title: "Firecracker golden from " + r.D.Name,
		cmds:  [][]string{kfireArgv("golden", r.D.Name)},
		warn:  "snapshots " + r.DS.Name + "@kfire (and -data); the VM stays as it is",
	}, nil
}

// fcTouched reports whether a plan ran kfire, so the caller can drop the
// ten-second instance cache and the next refresh shows the new state.
func fcTouched(p verbPlan) bool {
	for _, c := range p.cmds {
		for _, a := range c {
			if a == "kfire" {
				return true
			}
		}
		if len(c) >= 4 && c[2] == "systemctl" && strings.HasPrefix(c[4-1], "kfire-") {
			return true
		}
	}
	return false
}
