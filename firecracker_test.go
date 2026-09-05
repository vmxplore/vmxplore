package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// kfire's own JSON, as printed on onyx 2026-09-05, becomes estate rows with
// the fields the tree, dossier and verbs read.
func TestFCRowsFromKfireJSON(t *testing.T) {
	src := `[{"name":"web-stack-1","golden":"app-web-stack","mac":"aa:fc:ac:a1:b7:20","tap":"fc-web-stack-1",
	"bridge":"virbr0","vcpus":2,"ram_mb":2048,"port":80,"root_zvol":"rpool/vms/web-stack-1",
	"data_zvol":"rpool/vms/web-stack-1-data","created":"2026-09-05T05:52:00-07:00","state":"running","ip":"192.168.122.33"},
	{"name":"web-stack-2","golden":"app-web-stack","mac":"aa:fc:43:88:07:f3","tap":"fc-web-stack-2","bridge":"virbr0",
	"vcpus":2,"ram_mb":2048,"port":80,"root_zvol":"rpool/vms/web-stack-2","data_zvol":"","state":"shut off","ip":""}]`
	var insts []FCInstance
	if err := json.Unmarshal([]byte(src), &insts); err != nil {
		t.Fatal(err)
	}
	rows := fcRows(insts)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	r := rows[0]
	if r.FC == nil || r.Synthetic || r.Group != fcGroupLabel || r.D.Name != "web-stack-1" ||
		r.D.State != "running" || !r.D.Active || r.D.VCPUs != 2 || r.D.MaxMemKiB != 2048*1024 ||
		len(r.D.IPs) != 1 || r.D.IPs[0] != "192.168.122.33" || r.Backing != "rpool/vms/web-stack-1" {
		t.Errorf("row 0 = %+v", r)
	}
	if rows[1].D.Active || len(rows[1].D.IPs) != 0 || rows[1].FC.DataZvol != "" {
		t.Errorf("row 1 = %+v", rows[1])
	}
}

// The four verbs route to kfire; the rest refuse by name, so a click on
// "Snapshot" reads as a rule and not a bug.
func TestFCVerbsRouteToKfire(t *testing.T) {
	fcAsRoot = false
	up := rows1("running")
	down := rows1("shut off")
	want := func(p verbPlan, err error, argv string) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := strings.Join(p.cmds[0], " "); got != argv {
			t.Errorf("cmd = %q, want %q", got, argv)
		}
	}
	p, err := planStart(down)
	want(p, err, "sudo -n kfire start web-stack-1")
	p, err = planShutdown(up)
	want(p, err, "sudo -n kfire stop web-stack-1")
	p, err = planForceOff(up)
	want(p, err, "sudo -n systemctl stop kfire-web-stack-1")
	p, err = planDelete(up)
	want(p, err, "sudo -n kfire destroy web-stack-1")
	if !fcTouched(p) {
		t.Error("a kfire plan must invalidate the instance cache")
	}
	if _, err := planStart(up); err == nil {
		t.Error("start of a running microVM must refuse")
	}
	if _, err := planShutdown(down); err == nil {
		t.Error("shut down of a stopped microVM must refuse")
	}
	for name, f := range map[string]func(Row) (verbPlan, error){
		"snapshot":  func(r Row) (verbPlan, error) { return planSnapshot(r, "x") },
		"reboot":    planReboot,
		"suspend":   planSuspend,
		"clone":     func(r Row) (verbPlan, error) { return planClone(r, "y") },
		"specs":     func(r Row) (verbPlan, error) { return planSpecs(r, 2, 2) },
		"autostart": planAutostart,
	} {
		if _, err := f(up); err == nil || !strings.Contains(err.Error(), "Firecracker microVM") {
			t.Errorf("%s on a microVM: err = %v, want a refusal that names Firecracker", name, err)
		}
	}
	fcAsRoot = true
	if got := strings.Join(kfireArgv("list", "--json"), " "); got != "kfire list --json" {
		t.Errorf("as root: %q", got)
	}
	fcAsRoot = false
}

// A Firecracker golden comes from a shut-off appliance with a zvol; a
// running VM, a row with no dataset, and a microVM itself all refuse.
func TestPlanFCGoldenGuards(t *testing.T) {
	fcAsRoot = true
	ds := &Dataset{Name: "rpool/vms/app-web-stack"}
	if _, err := planFCGolden(Row{D: Dom{Name: "app-web-stack", State: "running"}, DS: ds}); err == nil ||
		!strings.Contains(err.Error(), "shut it down") {
		t.Errorf("running: %v", err)
	}
	if _, err := planFCGolden(Row{D: Dom{Name: "x", State: "shut off"}}); err == nil ||
		!strings.Contains(err.Error(), "no zvol") {
		t.Errorf("no dataset: %v", err)
	}
	if _, err := planFCGolden(rows1("shut off")); err == nil {
		t.Error("a microVM cannot be a golden")
	}
	if !kfireAvailable() {
		t.Skip("kfire not installed here")
	}
	p, err := planFCGolden(Row{D: Dom{Name: "app-web-stack", State: "shut off"}, DS: ds})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.cmds[0], " "); got != "kfire golden app-web-stack" {
		t.Errorf("cmd = %q", got)
	}
	fcAsRoot = false
}

func rows1(state string) Row {
	in := FCInstance{Name: "web-stack-1", Golden: "app-web-stack", State: state, VCPUs: 2, RAMMB: 2048,
		RootZvol: "rpool/vms/web-stack-1", DataZvol: "rpool/vms/web-stack-1-data"}
	return fcRows([]FCInstance{in})[0]
}
