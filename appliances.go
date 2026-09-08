// appliances.go — the Appliances catalog: push-button self-hosted apps.
//
// What it does, in order:
//  1. Holds a curated catalog of Appliance entries — each one a cloud-image
//     preset, a sizing default, a set of operator-facing fields, and a
//     fixed bash post-installer.
//  2. Renders an entry to a concrete post-install script: required fields
//     are checked, blank generate-fields get a crypto/rand secret, and every
//     value is emitted as a single-quoted bash assignment in a preamble.
//  3. Hands back a NewVMSpec the existing pipeline (newvm.go) builds
//     unchanged — so an appliance is just a New VM with the form pre-filled.
//
// Why: nearly every "how to self-host X" writeup is the same four moves —
// fetch a pinned artifact, write a config, init a database, drop a unit
// file. Encoding that once per app turns a weekend of following a blog post
// into a button, and Make Golden → Clone turns the result into a template.
//
// Notes: operator values are NEVER interpolated into the body of a script.
// The body is fixed bash that reads named variables; Render only prepends
// shell-quoted assignments. That is the whole injection story — a site name
// containing a quote, a `$(…)`, or a backtick is inert data, not code.
// Values are rejected if they contain a newline, since the scripts write
// them into line-oriented config formats.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ─── The catalog types ───────────────────────────────────────────────
//
// An Appliance is deliberately data, not code: a new entry is a struct
// literal plus a bash string, so adding an app never touches the pipeline,
// the GUI, or the tests. Validate is the one escape hatch, for the
// per-app rules that would otherwise only surface as a confusing failure
// deep inside the guest's first boot.

// ApplianceField is one operator-facing input on the appliance form.
//
// Generate means "if left blank, invent a strong value" — used for
// passwords and seeds so the happy path needs no typing. Secret only
// affects presentation (the GUI masks it); it does not change storage,
// since the rendered script necessarily contains the value in clear.
type ApplianceField struct {
	Key         string // variable name in the script; [A-Z0-9_]+
	Label       string // shown on the form
	Placeholder string
	Default     string
	Secret      bool // mask in the UI
	Generate    bool // blank → generated secret
	Required    bool
}

// Appliance is one catalog entry: everything needed to turn a stock cloud
// image into a running service, with no operator decisions beyond Fields.
type Appliance struct {
	Name     string // catalog key, shown in the picker
	Summary  string // one line, shown under the picker
	Homepage string
	License  string

	Distro string // key into cloudImages (newvm.go)
	VCPUs  int
	RAMMB  int
	DiskGB int

	Port    int    // primary service port, opened in the guest firewall
	LandsOn string // human hint: where the service appears once booted
	// ProbeTCP: the port speaks something other than HTTP (RDP, say), so
	// "up" is a TCP accept, not an HTTP response. Default false keeps the
	// stricter HTTP probe for everything that serves a page.
	ProbeTCP bool
	// ClientHint: lines the closing report prints under the landing line,
	// <vm-ip> substituted — what to run or set on the OPERATOR's machine
	// to get in with everything working. Born of the RDP tile: sound was
	// right in the guest and silent on the desk for an hour, because
	// Remmina defaults audio off and nothing said so (onyx, 2026-09-04).
	ClientHint []string

	// Needs is the substrate this recipe was written for. Recipes target
	// kldload (KVM + ZFS) by default; declaring NeedsZFS is what lets the
	// picker say "degraded" on a pool-less host instead of installing
	// something whose storage design silently did not happen.
	Needs Substrate

	// USB lists host USB devices (vendor:product, hex) to pass through to
	// the guest, attached live+persistent right after the build. An SDR or
	// TV tile is decorative without its radio. IDs absent from the host are
	// skipped with a warning — the appliance still builds, and plugging the
	// device in later plus `virsh attach-device` is the documented recovery.
	USB []string

	// DataGB attaches a second, blank disk of this size. app_pool_init in
	// the prologue turns it into the appliance's own pool, which is what
	// makes a recipe's dataset properties real rather than decorative.
	// Zero means no data disk.
	DataGB int

	Fields []ApplianceField

	// Validate runs before Render on the fully-defaulted value set. It
	// exists to fail fast on rules the guest would otherwise only report
	// from inside cloud-init, where nobody is watching.
	Validate func(vals map[string]string) error

	// Script is fixed bash. It reads the Fields by Key as shell variables
	// and must not interpolate anything else.
	Script string

	// Notes is operator-facing caveat text shown beside the form.
	Notes string
}

var fieldKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ─── Rendering ───────────────────────────────────────────────────────

// shellSingleQuote wraps s so bash sees it as one literal word, whatever
// it contains. Single quotes suppress every form of expansion, and the
// one character they cannot contain — the quote itself — is handled by
// closing the quote, emitting a backslash-escaped quote, and reopening.
// Bash concatenates adjacent quoted runs, so the result stays one word.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// randomSecret returns a URL-safe random string of about n characters,
// drawn from crypto/rand. Used for generate-fields (passwords, seeds).
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

// Defaults returns the field values an unedited form would submit.
func (a Appliance) Defaults() map[string]string {
	vals := make(map[string]string, len(a.Fields))
	for _, f := range a.Fields {
		vals[f.Key] = f.Default
	}
	return vals
}

// resolve fills in defaults and generated secrets, and enforces the
// invariants every appliance script depends on. Returns the completed
// value set; the caller's map is not modified.
//
// Failure modes: a required field left blank, a value containing a
// newline (scripts write these into line-oriented config), or a field key
// that is not a legal shell variable name.
func (a Appliance) resolve(vals map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(a.Fields))
	for _, f := range a.Fields {
		if !fieldKeyRE.MatchString(f.Key) {
			return nil, fmt.Errorf("appliance %s: bad field key %q", a.Name, f.Key)
		}
		v := strings.TrimSpace(vals[f.Key])
		if v == "" {
			v = f.Default
		}
		if v == "" && f.Generate {
			s, err := randomSecret(24)
			if err != nil {
				return nil, err
			}
			v = s
		}
		if v == "" && f.Required {
			return nil, fmt.Errorf("%s is required", f.Label)
		}
		if strings.ContainsAny(v, "\n\r") {
			return nil, fmt.Errorf("%s must be a single line", f.Label)
		}
		out[f.Key] = v
	}
	if a.Validate != nil {
		if err := a.Validate(out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Render produces the post-install bash for this appliance: a header, a
// preamble of shell-quoted assignments, then the fixed script body.
//
// The returned script is what lands in the guest as
// /var/lib/vmxplore-postinstall.sh and runs once, as root, on first boot.
func (a Appliance) Render(vals map[string]string) (string, error) {
	resolved, err := a.resolve(vals)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# vmxplore appliance: %s\n", a.Name)
	fmt.Fprintf(&b, "# %s\n", a.Summary)
	if a.Homepage != "" {
		fmt.Fprintf(&b, "# %s\n", a.Homepage)
	}
	b.WriteString("\n")
	// Field order, not map order — a rendered script must be byte-identical
	// for the same inputs so two operators can diff theirs.
	for _, f := range a.Fields {
		// EXPORTED, not plain assignments: recipes verify themselves with
		// `bash -c` one-liners, and a child shell never sees an unexported
		// variable. On smk-web every check that referenced a field failed
		// against a database that demonstrably existed.
		fmt.Fprintf(&b, "export %s=%s\n", f.Key, shellSingleQuote(resolved[f.Key]))
	}
	b.WriteString("\n")
	// The substrate prologue goes between the fields and the recipe: it reads
	// nothing from the operator, and every recipe below it depends on the
	// helpers it defines. Injected here so twelve recipes cannot drift from
	// twelve pasted copies of it.
	b.WriteString(strings.TrimRight(appliancePrologue, "\n"))
	b.WriteString("\n\n")
	b.WriteString(strings.TrimRight(a.Script, "\n"))
	b.WriteString("\n")
	return b.String(), nil
}

// Spec renders the appliance and returns the NewVMSpec that builds it.
// user/password are the guest login (not the app's admin account); the
// caller supplies them so the appliance form can stay app-focused.
func (a Appliance) Spec(vmName, user, password, sshKey string,
	vals map[string]string) (NewVMSpec, error) {
	script, err := a.Render(vals)
	if err != nil {
		return NewVMSpec{}, err
	}
	s := NewVMSpec{
		Name:     strings.TrimSpace(vmName),
		Distro:   a.Distro,
		VCPUs:    a.VCPUs,
		RAMMB:    a.RAMMB,
		DiskGB:   a.DiskGB,
		DataGB:   a.DataGB,
		User:     strings.TrimSpace(user),
		Password: password,
		SSHKey:   strings.TrimSpace(sshKey),
		PostInst: script,
	}
	return s, s.validate()
}

// ─── CLI surface ─────────────────────────────────────────────────────
//
// The rendered script is a useful artifact on its own: it is an ordinary
// bash installer with no vmxplore, libvirt or kldload dependency, so an
// upstream project can publish it as their own "install on a fresh VM"
// path. Printing it also makes the catalog reviewable without building a
// VM — you can read exactly what the button is about to run.

// PrintAppliances writes the catalog to w in operator-readable form.
func PrintAppliances(w *os.File) {
	for _, a := range Appliances() {
		fmt.Fprintf(w, "%s\n  %s\n", a.Name, a.Summary)
		fmt.Fprintf(w, "  %s · %s · %d vCPU, %d MB RAM, %d GB disk\n",
			a.License, a.Distro, a.VCPUs, a.RAMMB, a.DiskGB)
		fmt.Fprintf(w, "  serves: %s\n", a.LandsOn)
		// The same verdict the GUI colours by: what happens on THIS host.
		if level, blurb := ApplianceFit(a); blurb != "" {
			fmt.Fprintf(w, "  fit: [%s] %s\n", level, blurb)
		}
		for _, f := range a.Fields {
			req := ""
			if f.Required {
				req = " (required)"
			}
			fmt.Fprintf(w, "    %-14s %s%s\n", f.Key, f.Label, req)
		}
		fmt.Fprintln(w)
	}
}

// applianceOverrides parses KEY=VALUE arguments onto a value set. It
// rejects unknown keys rather than ignoring them, so a typo in a scripted
// invocation fails instead of silently installing the default.
func applianceOverrides(a Appliance, vals map[string]string,
	args []string) error {
	known := map[string]bool{}
	for _, f := range a.Fields {
		known[f.Key] = true
	}
	for _, arg := range args {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			return fmt.Errorf("expected KEY=VALUE, got %q", arg)
		}
		if !known[k] {
			return fmt.Errorf("%s has no field %q", a.Name, k)
		}
		vals[k] = v
	}
	return nil
}

// WaitAppliance blocks until a freshly built appliance answers on its
// port, and returns the URL it answered on.
//
// Why this exists: the pipeline returns as soon as the domain is defined,
// but the appliance is not usable until cloud-init has run the installer —
// a minute or three later, on an address nobody knows yet. Without this
// the operator is handed "http://<vm-ip>/" and has to go hunt for the
// lease, which is precisely the seam that stops a deploy feeling like one
// action.
//
// Args: name is the domain; port is the appliance's Port; progress gets a
// line per phase. Returns the URL, or an error describing which phase
// timed out — the two phases fail for entirely different reasons (no DHCP
// lease vs. an installer that died), so they are reported separately.
func WaitAppliance(name string, port int, progress func(string)) (string, error) {
	return waitAppliance(context.Background(), name, port, false, progress)
}

// WaitApplianceTCP is WaitAppliance for a port that does not speak HTTP:
// ready means the guest accepts a TCP connection on it.
func WaitApplianceTCP(name string, port int, progress func(string)) (string, error) {
	return waitAppliance(context.Background(), name, port, true, progress)
}

// waitAppliance returns early with ctx's error when the caller cancels —
// this is the long pole of a build (a first boot is minutes), so it is the
// wait a Cancel button or a Ctrl-C actually interrupts.
func waitAppliance(ctx context.Context, name string, port int, tcp bool, progress func(string)) (string, error) {
	const (
		leaseTimeout = 3 * time.Minute
		// 25m, not 10: a NeedsZFS recipe on a stock cloud image now
		// INSTALLS ZFS first, and the dkms build alone is 5-8 minutes on
		// two vCPUs before the app's own install starts. The golden fast
		// path (seal a built appliance, clone clones) is what brings this
		// back to seconds.
		bootTimeout = 25 * time.Minute
	)
	lv, err := ConnectSystem()
	if err != nil {
		return "", err
	}
	defer lv.Close()

	progress("waiting for " + name + " to get an address")
	var ip string
	for deadline := time.Now().Add(leaseTimeout); time.Now().Before(deadline); {
		if ips, err := lv.LeaseIPs(name); err == nil && len(ips) > 0 {
			ip = ips[0]
			break
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s: %w", name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	if ip == "" {
		return "", fmt.Errorf("%s got no DHCP lease within %s — is its "+
			"network up?", name, leaseTimeout)
	}

	url := fmt.Sprintf("http://%s/", ip)
	if port != 80 && port != 0 {
		url = fmt.Sprintf("http://%s:%d/", ip, port)
	}
	progress("at " + ip + " — waiting for the first boot to finish installing")

	// Any HTTP response counts as ready: a redirect or even a 404 means
	// the service is listening, and only the appliance knows what its own
	// landing page should be. A TCP tile (RDP) is ready when the port
	// accepts, and its URL is not http:// at all.
	if tcp {
		url = fmt.Sprintf("%s:%d", ip, port)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for deadline := time.Now().Add(bootTimeout); time.Now().Before(deadline); {
		if tcp {
			if c, err := net.DialTimeout("tcp", url, 5*time.Second); err == nil {
				c.Close()
				return url, nil
			}
		} else if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			return url, nil
		}
		select {
		case <-ctx.Done():
			return url, fmt.Errorf("%s: %w", name, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	return url, fmt.Errorf("%s never answered on %s within %s — check "+
		"`journalctl -u cloud-final` in the guest", name, url, bootTimeout)
}

// applianceFlags are the non-KEY=VALUE options --appliance accepts. They
// describe the *guest* (its login), never the app — app configuration is
// the catalog entry's Fields, so the flag set never grows per appliance.
type applianceFlags struct {
	vm       string
	user     string
	password string
	sshKey   string
	noWait   bool
	golden   bool // seal as a clone template instead of enrolling
	// distro overrides the tile's cloud image (a key into cloudImages):
	// every substrate recipe carries an apt branch and an rpm branch, and
	// a tile pinned to one image never exercises the other. The Web Stack
	// was proven on Fedora only until this flag (2026-09-05).
	distro string
	rest   []string
}

// parseApplianceFlags splits argv into guest options and KEY=VALUE pairs.
// Defaults mirror the GUI dialog so both surfaces build the same VM.
func parseApplianceFlags(args []string) (applianceFlags, error) {
	f := applianceFlags{user: "admin"}
	if b, err := os.ReadFile(os.Getenv("HOME") + "/.ssh/id_ed25519.pub"); err == nil {
		f.sshKey = strings.TrimSpace(string(b))
	}
	need := func(i int, what string) (string, error) {
		if i >= len(args) {
			return "", fmt.Errorf("%s needs a value", what)
		}
		return args[i], nil
	}
	for i := 0; i < len(args); i++ {
		var err error
		switch args[i] {
		case "--vm", "--name":
			i++
			f.vm, err = need(i, args[i-1])
		case "--user":
			i++
			f.user, err = need(i, "--user")
		case "--password":
			i++
			f.password, err = need(i, "--password")
		case "--no-wait":
			f.noWait = true
		case "--golden":
			f.golden = true
		case "--distro":
			i++
			if f.distro, err = need(i, "--distro"); err == nil {
				if _, ok := cloudImages[f.distro]; !ok {
					err = fmt.Errorf("--distro %q: not a cloud image key (see --images)", f.distro)
				}
			}
		case "--ssh-key":
			i++
			var p string
			if p, err = need(i, "--ssh-key"); err == nil {
				var b []byte
				if b, err = os.ReadFile(p); err == nil {
					f.sshKey = strings.TrimSpace(string(b))
				}
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				return f, fmt.Errorf("unknown option %q", args[i])
			}
			f.rest = append(f.rest, args[i])
		}
		if err != nil {
			return f, err
		}
	}
	if f.vm == "" {
		return f, fmt.Errorf("--vm NAME is required")
	}
	return f, nil
}

// RunApplianceBuild deploys one catalog entry as a VM and streams the
// pipeline's steps. This is the headless twin of Build ▸ Appliance… —
// the path someone takes who installed vmxplore five minutes ago and has
// no interest in finding a menu.
//
// Returns a process exit status. Progress goes to stderr so stdout stays
// free for the final URL, which makes the command pipeable.
func RunApplianceBuild(name string, args []string) int {
	a, ok := ApplianceByName(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "vmx: no appliance %q — try --appliances\n", name)
		return 2
	}
	f, err := parseApplianceFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 2
	}
	if f.distro != "" {
		a.Distro = f.distro
	}
	vals := a.Defaults()
	if err := applianceOverrides(a, vals, f.rest); err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 2
	}
	if f.password == "" && f.sshKey == "" {
		// Not fatal — but a guest you cannot log into is almost never what
		// was meant, and the app's own admin account is a separate thing.
		fmt.Fprintln(os.Stderr,
			"vmx: warning: no guest password or ssh key — you will not be "+
				"able to log into the VM itself")
	}
	spec, err := a.Spec(f.vm, f.user, f.password, f.sshKey, vals)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 2
	}

	// The ZFS parent is a best-effort optimisation: with it the disk is a
	// sparse zvol that clones instantly, without it a qcow2 file. Never a
	// hard failure — this must work on a plain libvirt box.
	parent := ""
	if lv, err := ConnectSystem(); err == nil {
		defer lv.Close()
		if doms, err := lv.Estate(); err == nil && HasZFS() {
			dss, _ := ListDatasets()
			snaps, _ := ListSnapshots()
			rs, _ := LoadRules("") // built-in profile is fine for grouping
			var rows []Row
			for _, g := range BuildEstate(doms, dss, snaps, rs,
				LoadAnnotations()) {
				rows = append(rows, g.Rows...)
			}
			parent = ZFSVMParent(rows)
		}
	}

	// On a kldload host the appliance is born enrolled: mesh, estate cert,
	// inventory. Seeding the host ops key for root is what makes the guest
	// reachable for that; on kvm/kvm+zfs tiers nothing is seeded.
	if KldloadTier() == "kldload" {
		if k := hostOpsPubkey(); k != "" {
			spec.RootSSHKeys = append(spec.RootSSHKeys, k)
		}
	}
	log := func(line string) { fmt.Fprintln(os.Stderr, line) }
	if err := BuildNewVM(spec, parent, log); err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 1
	}
	// Radios and tuners attach the moment the guest exists, so the recipe
	// running in cloud-init already sees them.
	AttachUSBDevices(spec.Name, a.USB, log)
	if f.noWait {
		fmt.Fprintf(os.Stderr, "\n%s is building %s — it will serve on %s\n",
			spec.Name, a.Name, a.LandsOn)
		if KldloadTier() == "kldload" {
			fmt.Fprintln(os.Stderr,
				"substrate enrollment skipped under --no-wait — it needs the guest up")
		}
		return 0
	}
	url, err := waitAppliance(context.Background(), spec.Name, a.Port, a.ProbeTCP, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 1
	}
	if f.golden {
		// A template, not a member: no mesh key, no cert, no inventory row
		// — every one of those is an identity, and a clone must mint its
		// own. The port wait above is the proof the recipe finished.
		if err := SealApplianceGolden(spec.Name, log); err != nil {
			fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "\n%s is a golden — right-click → Clone in the GUI, "+
			"or clone from %s@golden, clones out ready copies.\n", a.Name, spec.Name)
		fmt.Println(spec.Name + "@golden")
		return 0
	}
	// The service answers, so cloud-init has finished and root ssh is live —
	// the cheapest moment to enroll.
	EnrollAppliance(spec.Name, applianceSlug(a.Name), log)
	fmt.Fprintf(os.Stderr, "\n%s is ready. Credentials are in "+
		"/root/ inside the guest.\n", a.Name)
	fmt.Println(url)
	return 0
}

// SealApplianceGolden turns a finished appliance build into a clone
// template: the domain's estate row (root zvol and, through MakeGolden, its
// -data disk) is shut down, sealed and snapshotted @golden. Needs ZFS —
// there is no qcow2 golden — and reports that plainly.
//
// This is the demo: build a database stack with one button in a few
// minutes, then turn around and clone a cluster of them in a second.
func SealApplianceGolden(vm string, log func(string)) error {
	r, ok := rowForDomain(vm)
	if !ok {
		return fmt.Errorf("%s: built, but libvirt does not list it — cannot seal", vm)
	}
	return MakeGolden(r, log)
}

// RunApplianceScript renders one catalog entry to stdout. Returns a
// process exit status.
func RunApplianceScript(name string, args []string) int {
	a, ok := ApplianceByName(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "vmx: no appliance %q — try --appliances\n", name)
		return 2
	}
	vals := a.Defaults()
	if err := applianceOverrides(a, vals, args); err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 2
	}
	script, err := a.Render(vals)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %v\n", err)
		return 1
	}
	fmt.Print("#!/usr/bin/env bash\nset -Eeuo pipefail\n\n", script)
	return 0
}

// ─── The catalog ─────────────────────────────────────────────────────

// Appliances returns the catalog in menu order.
func Appliances() []Appliance { return applianceCatalog }

// ApplianceByName looks up one entry. ok is false for an unknown name.
func ApplianceByName(name string) (Appliance, bool) {
	for _, a := range applianceCatalog {
		if a.Name == name {
			return a, true
		}
	}
	return Appliance{}, false
}

// ApplianceNames lists the catalog keys in menu order (for the picker).
func ApplianceNames() []string {
	out := make([]string, 0, len(applianceCatalog))
	for _, a := range applianceCatalog {
		out = append(out, a.Name)
	}
	return out
}

// The home-lab presets (homelab.go) come after the blogging pair so the
// picker opens on what the catalog started as; order here IS the order in
// the GUI list and in `vmx appliances`.
var applianceCatalog = []Appliance{
	{
		Name:     "Web Stack",
		Summary:  "nginx + PHP-FPM in front of PostgreSQL and Valkey on their own pool, with a live example page",
		Homepage: "https://nginx.org",
		License:  "BSD-2-Clause (nginx), PostgreSQL, BSD-3-Clause (Redis)",

		Distro: "fedora",
		VCPUs:  2,
		RAMMB:  2048,
		DiskGB: 20,

		// The database is the reason this wants a pool: an 8K-record dataset
		// matching PostgreSQL's page size, snapshots before schema changes,
		// and a rollback that is one command. Degrades to plain dirs.
		Needs:  NeedsZFS,
		DataGB: 50,

		Port:    80,
		LandsOn: "http://<vm-ip>/  (stack health at /healthz)",

		Notes: "The three tiers every web application starts with, configured the " +
			"way you would configure them by hand and then verified.\n\n" +
			"PostgreSQL and Redis both listen on loopback ONLY and are never " +
			"reachable from outside the VM; nginx is the single public surface. " +
			"Redis takes a password anyway, because a loopback bind is one " +
			"misconfigured proxy away from not being one.\n\n" +
			"nginx proxies / to the upstream port you nominate, so you drop your " +
			"own application on that port and it is already fronted, gzipped and " +
			"behind sane security headers. Until something listens there, / " +
			"returns 502 by design — /healthz is what tells you the stack " +
			"itself is up, and it proves it by actually querying both " +
			"databases rather than reporting that a unit is active.\n\n" +
			"Leave the domain blank for plain HTTP, which is right for a LAN VM " +
			"or one behind your own edge proxy. Set it and certbot requests a " +
			"Let's Encrypt certificate on first boot, which needs the name to " +
			"already resolve here from the public internet with 80/443 open.",

		Fields: []ApplianceField{
			{Key: "WS_POOL", Label: "pool name",
				Placeholder: "created on the appliance's data disk",
				Default:     "tank", Required: true},
			{Key: "WS_ALLOW_CIDR", Label: "allowed source",
				Placeholder: "who may reach http/https",
				Default:     "192.168.0.0/16", Required: true},
			{Key: "WS_DB_NAME", Label: "database name", Default: "appdb", Required: true},
			{Key: "WS_DB_USER", Label: "database user", Default: "appuser", Required: true},
			{Key: "WS_DB_PASS", Label: "database password",
				Placeholder: "blank = generate one", Secret: true,
				Generate: true, Required: true},
			{Key: "WS_REDIS_PASS", Label: "redis password",
				Placeholder: "blank = generate one", Secret: true,
				Generate: true, Required: true},
			{Key: "WS_UPSTREAM_PORT", Label: "upstream port nginx proxies to",
				Default: "8080", Required: true},
			{Key: "WS_DOMAIN", Label: "public domain (optional, enables HTTPS)",
				Placeholder: "app.example.com"},
			{Key: "WS_TLS_EMAIL", Label: "email for certificate notices (optional)",
				Placeholder: "you@example.com"},
		},

		Validate: func(v map[string]string) error {
			if !webStackIdentRE.MatchString(v["WS_DB_NAME"]) {
				return fmt.Errorf("database name %q must be lowercase letters, digits and underscores, starting with a letter", v["WS_DB_NAME"])
			}
			if !webStackIdentRE.MatchString(v["WS_DB_USER"]) {
				return fmt.Errorf("database user %q must be lowercase letters, digits and underscores, starting with a letter", v["WS_DB_USER"])
			}
			if len(v["WS_DB_PASS"]) < 8 {
				return fmt.Errorf("database password must be at least 8 characters")
			}
			if len(v["WS_REDIS_PASS"]) < 8 {
				return fmt.Errorf("redis password must be at least 8 characters")
			}
			// Both passwords land in config files (pg_hba-adjacent SQL, a
			// redis.conf line); a quote or whitespace would not escalate —
			// psql gets them via :'var' binding — but it WOULD produce a
			// server that silently rejects the credential it was built with.
			for _, k := range []string{"WS_DB_PASS", "WS_REDIS_PASS"} {
				if strings.ContainsAny(v[k], " \t\n'\"") {
					return fmt.Errorf("%s must not contain spaces or quotes", k)
				}
			}
			if err := checkPoolName(v["WS_POOL"]); err != nil {
				return err
			}
			// The address goes straight to certbot; a typo'd one costs an
			// ACME round trip and a cryptic failure inside the guest.
			if e := v["WS_TLS_EMAIL"]; e != "" && !applianceEmailRE.MatchString(e) {
				return fmt.Errorf("certificate email %q does not look like an address", e)
			}
			port, err := strconv.Atoi(v["WS_UPSTREAM_PORT"])
			if err != nil || port < 1024 || port > 65535 {
				return fmt.Errorf("upstream port %q must be a number between 1024 and 65535", v["WS_UPSTREAM_PORT"])
			}
			if port == 80 || port == 443 {
				return fmt.Errorf("upstream port %d collides with nginx itself", port)
			}
			if d := v["WS_DOMAIN"]; d != "" && strings.Contains(d, "/") {
				return fmt.Errorf("domain %q must be a bare hostname, not a URL", d)
			}
			if v["WS_DOMAIN"] == "" && v["WS_TLS_EMAIL"] != "" {
				return fmt.Errorf("a certificate email without a domain has nothing to certify — set the domain too")
			}
			return nil
		},

		Script: webStackScript,
	},
	lampStack,
	jellyfin, plex, seedbox, icecast, sdrStation, tvheadend,
	adguardHome, syncthing,
	vdiDesktop, rdpDesktop,
}

// webStackIdentRE keeps database and role names to what PostgreSQL accepts
// unquoted, so the script never has to quote an identifier it was handed.
var webStackIdentRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// webStackScript is fixed bash — it reads WS_* from the preamble Render
// prepends and interpolates nothing else.
//
// Three tiers, each configured rather than defaulted:
//
//	postgres  loopback-only, scram-sha-256, a dedicated role and database,
//	          shared_buffers sized from the VM's actual RAM
//	redis     loopback-only AND password-protected, maxmemory with an
//	          eviction policy so it cannot OOM the box it shares
//	php-fpm   the example page and /healthz: each request opens PostgreSQL
//	          as the app user and the cache over RESP, so the page is the
//	          proof and /healthz is 200 only when both stores answered
//	nginx     the only public surface: the page at /, the operator's own
//	          app proxied at /app/, security headers
//
// Every service is installed and enabled in the same breath, because a unit
// that ships without being enabled is a service that works until the first
// reboot. Each tier is asserted by OUTCOME at the end — pg_isready, a real
// Redis AUTH+PING, nginx -t, and an HTTP fetch of /healthz — since apt
// returning 0 says nothing about whether the thing runs.
const webStackScript = `
APP_TAG=webstack
APP_POOL="$WS_POOL"

app_pool_init

# ─── datasets BEFORE packages, so initdb lands inside them ──────────────────
# PostgreSQL writes 8K pages; a matching recordsize means one page per record
# instead of read-modify-write cycles on 128K blocks. The families keep their
# own data roots, so the mountpoint follows the family.
if [ "$APP_FAMILY" = rpm ]; then _pgroot=/var/lib/pgsql; else _pgroot=/var/lib/postgresql; fi
app_dataset pgdata "$_pgroot"        recordsize=8K
# valkey on Fedora, redis on Debian — one dataset serves whichever,
# mounted where that family's daemon keeps its state.
app_dataset redis "$([ "$APP_FAMILY" = rpm ] && echo /var/lib/valkey || echo /var/lib/redis)" recordsize=16K
app_dataset www    /var/www          compression=zstd

# ─── one transaction per tier ───────────────────────────────────────────────
# A single install with everything in it is all-or-nothing: one unavailable
# name takes the whole stack down and the operator gets a wall of dependency
# output instead of "redis is missing".
if [ "$APP_FAMILY" = rpm ]; then
    app_pkg nginx
    app_pkg postgresql-server
    _pgsvc=postgresql
    # The P in LAMP: php-fpm serves the example page and /healthz, both of
    # which open PostgreSQL and the cache on every request. Fedora's pool
    # already lists nginx in listen.acl_users (php-fpm 8.5, checked
    # 2026-09-05); no phpredis here — the page speaks RESP over a socket.
    app_pkg php-fpm
    app_pkg php-pgsql
    _fpmsvc=php-fpm; _fpmsock=/run/php-fpm/www.sock; _fpmgrp=apache
    # Fedora 41+ replaced redis with valkey; "dnf install redis" exits 0 via
    # the virtual provide while installing NO redis RPM. app_pkg's artefact
    # check caught exactly that on this recipe's first live run (smk-web,
    # 2026-09-04): "transaction returned 0 but these are NOT installed:
    # redis". Install what the distro actually ships.
    if dnf -q info valkey >/dev/null 2>&1; then
        app_pkg valkey
        _redsvc=valkey; _reduser=valkey; _redcli=valkey-cli
        _redconfdir=/etc/valkey; _redconf=/etc/valkey/valkey.conf
    else
        app_pkg redis
        _redsvc=redis; _reduser=redis; _redcli=redis-cli
        _redconfdir=/etc/redis; _redconf=/etc/redis/redis.conf
    fi
else
    app_pkg nginx
    app_pkg postgresql
    app_pkg redis-server
    app_pkg php-fpm
    app_pkg php-pgsql
    _pgsvc=postgresql
    _redsvc=redis-server; _reduser=redis; _redcli=redis-cli
    _redconfdir=/etc/redis; _redconf=/etc/redis/redis.conf
    # Debian names the unit and the socket by PHP version (php8.4-fpm on
    # trixie); read both off the pool file rather than pin a version.
    _fpmpool=$(ls /etc/php/*/fpm/pool.d/www.conf 2>/dev/null | head -1)
    _fpmsock=$(sed -n 's/^listen *= *//p' "$_fpmpool" 2>/dev/null | head -1)
    _fpmsvc=$(basename "$(ls /lib/systemd/system/php*-fpm.service 2>/dev/null | head -1)" .service)
    _fpmgrp=www-data
    [ -n "$_fpmsock" ] && [ -n "$_fpmsvc" ] || app_die "php-fpm installed but no pool file / unit found"
fi

# ─── postgres ───────────────────────────────────────────────────────────────
if [ "$APP_FAMILY" = rpm ] && [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then
    postgresql-setup --initdb >/dev/null 2>&1 || app_die "initdb failed"
fi
app_enable "$_pgsvc"
_i=0
until sudo -u postgres psql -tAc 'SELECT 1' >/dev/null 2>&1; do
    _i=$((_i + 1)); [ "$_i" -lt 30 ] || app_die "postgres never accepted connections"
    sleep 1
done

# On ZFS every write is copy-on-write and torn pages cannot happen, so
# full_page_writes buys nothing and doubles WAL volume. Only when the data
# actually sits on the pool.
if [ -n "${APP_POOL:-}" ]; then
    sudo -u postgres psql -c "ALTER SYSTEM SET full_page_writes = off" >/dev/null
    sudo -u postgres psql -c "SELECT pg_reload_conf()" >/dev/null
fi

# Role and database, idempotently, with psql's :'var' binding so the password
# is quoted by psql itself rather than interpolated into SQL text.
# The statement goes in on STDIN, not -c: psql only interpolates :'var'
# bindings in input it reads, and -c text is sent to the server verbatim —
# the literal :'pw' produced "syntax error at or near :". Proven live on
# smk-web 2026-09-04; the stdin form created the role with the same
# binding. The binding is still the point: psql quotes the password, so
# it never becomes SQL text.
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='$WS_DB_USER'" | grep -q 1 ||
    echo "CREATE ROLE $WS_DB_USER LOGIN PASSWORD :'pw';" |
    sudo -u postgres psql -v pw="$WS_DB_PASS" >/dev/null
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='$WS_DB_NAME'" | grep -q 1 ||
    sudo -u postgres createdb -O "$WS_DB_USER" "$WS_DB_NAME"

# REPLACE the localhost auth method, never append. pg_hba is first-match-
# wins: Fedora ships "host all all 127.0.0.1/32 ident" and a scram line
# appended below it is dead text — the app user failed ident auth on
# smk-web while the "fixed" hba looked complete at the bottom of the file.
_hba="$(sudo -u postgres psql -tAc 'SHOW hba_file' | tr -d ' ')"
if grep -qE '^host\s+all\s+all\s+(127\.0\.0\.1/32|::1/128)\s+ident' "$_hba"; then
    sed -i -E 's#^(host\s+all\s+all\s+(127\.0\.0\.1/32|::1/128)\s+)ident#\1scram-sha-256#' "$_hba"
    systemctl reload "$_pgsvc" 2>/dev/null || systemctl restart "$_pgsvc"
elif ! grep -qE '^host\s+all\s+all\s+127\.0\.0\.1/32\s+(scram-sha-256|md5)' "$_hba"; then
    echo 'host all all 127.0.0.1/32 scram-sha-256' >>"$_hba"
    systemctl reload "$_pgsvc" 2>/dev/null || systemctl restart "$_pgsvc"
fi

# ─── redis ──────────────────────────────────────────────────────────────────
_reddir=/var/lib/$_reduser
install -d -m 0750 -o "$_reduser" -g "$_reduser" "$_reddir" 2>/dev/null ||
    install -d -m 0750 "$_reddir"
install -d -m 0755 "$_redconfdir"
if [ -f "$_redconf" ] && ! grep -q '^# kldload appliance' "$_redconf"; then
    cp -n "$_redconf" "$_redconf.dist"
fi
cat >"$_redconf" <<REDIS
# kldload appliance — regenerated by the Web Stack recipe; original in redis.conf.dist
bind 127.0.0.1 -::1
port 6379
requirepass $WS_REDIS_PASS
appendonly yes
dir $_reddir
REDIS
chown "$_reduser:$_reduser" "$_redconf" 2>/dev/null || true
chmod 0640 "$_redconf"
chown -R "$_reduser:$_reduser" "$_reddir" 2>/dev/null || true
app_selinux redis_var_lib_t "${_reddir}(/.*)?"
app_relabel "$_reddir"
app_enable "$_redsvc"
systemctl restart "$_redsvc" 2>/dev/null || true

# ─── the example page ───────────────────────────────────────────────────────
# A landing page that PROVES the stack rather than announces it: every
# visit opens PostgreSQL as the app user and writes a visit row, bumps a
# counter in the cache over its own wire protocol, and prints this
# instance's hostname, address and uptime. Ten cloned copies each show
# their own name and counts, which is the demo ("wouldn't it be better to
# have a fully built example page", operator, 2026-09-05). The credentials
# live in a config the fpm user can read and nobody else can.
install -d -m 0755 /etc/webstack /var/www/app
cat >/etc/webstack/config.php <<PHPCONF
<?php
// kldload Web Stack — written by the recipe; the example page and /healthz read it.
return [
  'db_name' => '${WS_DB_NAME}',
  'db_user' => '${WS_DB_USER}',
  'db_pass' => '${WS_DB_PASS}',
  'cache_pass' => '${WS_REDIS_PASS}',
  'upstream_port' => '${WS_UPSTREAM_PORT}',
];
PHPCONF
chown root:"$_fpmgrp" /etc/webstack/config.php
chmod 0640 /etc/webstack/config.php
cat >/var/www/app/stack.php <<'PHP'
<?php
// stack.php — the checks the page and /healthz share. Each returns an
// array with 'ok' plus what it learned; a failure carries its message.
function ws_config(): array { return require '/etc/webstack/config.php'; }

function ws_postgres(array $c, bool $write): array {
  $t = microtime(true);
  try {
    $pdo = new PDO("pgsql:host=127.0.0.1;port=5432;dbname={$c['db_name']}", $c['db_user'], $c['db_pass'],
      [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION, PDO::ATTR_TIMEOUT => 3]);
    $pdo->exec('CREATE TABLE IF NOT EXISTS visits (id serial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), client text, host text)');
    if ($write) {
      $st = $pdo->prepare('INSERT INTO visits (client, host) VALUES (?, ?)');
      $st->execute([$_SERVER['REMOTE_ADDR'] ?? '', gethostname()]);
    }
    $count = (int)$pdo->query('SELECT count(*) FROM visits')->fetchColumn();
    $last = $pdo->query('SELECT to_char(max(at), \'YYYY-MM-DD HH24:MI:SS TZ\') FROM visits')->fetchColumn();
    $ver = $pdo->query('SELECT version()')->fetchColumn();
    return ['ok' => true, 'version' => preg_replace('/ on .*/', '', $ver), 'visits' => $count,
            'last' => $last, 'ms' => round((microtime(true) - $t) * 1000, 1)];
  } catch (Throwable $e) {
    return ['ok' => false, 'error' => $e->getMessage()];
  }
}

// RESP by hand: AUTH, INCR, PING. No extension to package on either family.
function ws_resp($s, array $args) {
  $out = '*' . count($args) . "\r\n";
  foreach ($args as $a) { $out .= '$' . strlen($a) . "\r\n" . $a . "\r\n"; }
  fwrite($s, $out);
  $line = fgets($s);
  if ($line === false) { throw new RuntimeException('no reply'); }
  $line = rtrim($line, "\r\n");
  switch ($line[0]) {
    case '-': throw new RuntimeException(substr($line, 1));
    case '+': case ':': return substr($line, 1);           // simple string, integer
    case '$':                                              // bulk string: length, then the payload line
      if ($line === '$-1') { return null; }
      $payload = fgets($s);
      return $payload === false ? null : rtrim($payload, "\r\n");
  }
  throw new RuntimeException("unexpected reply: $line");
}

function ws_cache(array $c, bool $write): array {
  $t = microtime(true);
  try {
    $s = @stream_socket_client('tcp://127.0.0.1:6379', $errno, $errstr, 3);
    if (!$s) { throw new RuntimeException("connect: $errstr"); }
    stream_set_timeout($s, 3);
    ws_resp($s, ['AUTH', $c['cache_pass']]);
    $key = 'hits:' . gethostname();
    $hits = $write ? (int)ws_resp($s, ['INCR', $key]) : (int)ws_resp($s, ['GET', $key]);
    $pong = ws_resp($s, ['PING']);
    fclose($s);
    return ['ok' => $pong === 'PONG', 'hits' => $hits, 'ms' => round((microtime(true) - $t) * 1000, 1)];
  } catch (Throwable $e) {
    return ['ok' => false, 'error' => $e->getMessage()];
  }
}
PHP
cat >/var/www/app/healthz.php <<'PHP'
<?php
// /healthz — 200 only when PostgreSQL and the cache both answered, 503
// otherwise, with what failed. Reads, never writes: a probe is not a visit.
require '/var/www/app/stack.php';
$c = ws_config();
$pg = ws_postgres($c, false);
$ca = ws_cache($c, false);
$ok = $pg['ok'] && $ca['ok'];
http_response_code($ok ? 200 : 503);
header('Content-Type: application/json');
echo json_encode(['ok' => $ok, 'host' => gethostname(), 'postgres' => $pg, 'cache' => $ca]), "\n";
PHP
cat >/var/www/app/index.php <<'PHP'
<?php
// The example page. See stack.php for what each visit does.
require '/var/www/app/stack.php';
$c = ws_config();
// A visit is a GET of the page itself. The favicon a browser asks for on
// every load, a HEAD probe, a monitor's poll of a path that falls through
// to this script: none of those is someone looking at the page, and each
// one used to count ("the hit counter going up too fast", 2026-09-05).
$visit = ($_SERVER['REQUEST_METHOD'] ?? '') === 'GET' && strtok($_SERVER['REQUEST_URI'] ?? '/', '?') === '/';
$pg = ws_postgres($c, $visit);
$ca = ws_cache($c, $visit);
$host = gethostname();
$ip = $_SERVER['SERVER_ADDR'] ?? '';
$up = (int)explode(' ', (string)@file_get_contents('/proc/uptime'))[0];
$uptime = sprintf('%dd %02dh %02dm', intdiv($up, 86400), intdiv($up % 86400, 3600), intdiv($up % 3600, 60));
$cores = max(1, (int)substr_count((string)@file_get_contents('/proc/cpuinfo'), "\nprocessor"), (int)str_starts_with((string)@file_get_contents('/proc/cpuinfo'), 'processor'));
$mem = 0; if (preg_match('/MemTotal:\s+(\d+)/', (string)@file_get_contents('/proc/meminfo'), $m)) { $mem = round($m[1] / 1024); }
$mark = fn(bool $ok) => $ok ? '<span class="ok">●</span>' : '<span class="bad">●</span>';
$h = fn($v) => htmlspecialchars((string)$v, ENT_QUOTES);
?>
<!doctype html>
<!-- web stack up -->
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Web Stack · <?= $h($host) ?></title>
<style>
:root{color-scheme:dark}body{margin:0;background:#0f1216;color:#d7dde5;font:15px/1.5 system-ui,sans-serif}
main{max-width:860px;margin:0 auto;padding:2.5rem 1.5rem}h1{font-size:1.6rem;margin:0 0 .2rem}h1 small{color:#8a94a3;font-weight:400;font-size:1rem;margin-left:.6rem}
.sub{color:#8a94a3;margin:0 0 2rem}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(250px,1fr));gap:1rem}
.card{background:#171c23;border:1px solid #242b35;border-radius:10px;padding:1rem 1.2rem}.card h2{font-size:.85rem;letter-spacing:.06em;text-transform:uppercase;color:#8a94a3;margin:0 0 .6rem}
.big{font-size:2rem;font-weight:600;margin:.1rem 0}.kv{display:grid;grid-template-columns:auto 1fr;gap:.15rem .8rem;font-family:ui-monospace,monospace;font-size:.85rem}.kv b{color:#8a94a3;font-weight:400}
.ok{color:#5fd38d}.bad{color:#ff6b6b}.err{color:#ff6b6b;font-family:ui-monospace,monospace;font-size:.85rem}
footer{margin-top:2rem;color:#8a94a3;font-size:.85rem}footer a{color:#8fb4ff}code{color:#c9d3e0}
</style></head><body><main>
<h1>Web Stack <small><?= $h($host) ?></small></h1>
<p class="sub">nginx → PHP-FPM → PostgreSQL + Valkey (Redis on Debian), on its own ZFS pool. Every visit writes a row and bumps a counter — reload to watch.</p>
<div class="grid">
<div class="card"><h2>this instance</h2>
<div class="kv"><b>host</b><span><?= $h($host) ?></span><b>address</b><span><?= $h($ip) ?></span><b>uptime</b><span><?= $uptime ?></span>
<b>cpu / ram</b><span><?= $cores ?> core<?= $cores == 1 ? '' : 's' ?> / <?= $mem ?> MB</span><b>kernel</b><span><?= $h(php_uname('r')) ?></span>
<b>web</b><span><?= $h($_SERVER['SERVER_SOFTWARE'] ?? 'nginx') ?> · PHP <?= PHP_VERSION ?></span></div></div>
<div class="card"><h2><?= $mark($pg['ok']) ?> PostgreSQL</h2>
<?php if ($pg['ok']): ?><div class="big"><?= $pg['visits'] ?></div><div class="kv"><b>visits</b><span>rows in <code>visits</code></span><b>last</b><span><?= $h($pg['last']) ?></span><b>server</b><span><?= $h($pg['version']) ?></span><b>round trip</b><span><?= $pg['ms'] ?> ms</span></div>
<?php else: ?><p class="err"><?= $h($pg['error']) ?></p><?php endif ?></div>
<div class="card"><h2><?= $mark($ca['ok']) ?> Valkey / Redis</h2>
<?php if ($ca['ok']): ?><div class="big"><?= $ca['hits'] ?></div><div class="kv"><b>hits</b><span><code>INCR hits:<?= $h($host) ?></code></span><b>auth</b><span>required, loopback only</span><b>round trip</b><span><?= $ca['ms'] ?> ms</span></div>
<?php else: ?><p class="err"><?= $h($ca['error']) ?></p><?php endif ?></div>
</div>
<footer><a href="/healthz">/healthz</a> answers 200 only when both stores do · your app goes behind <a href="/app/">/app/</a> (proxied to 127.0.0.1:<?= $h($c['upstream_port']) ?>) · built by <a href="https://kldload.com">kldload</a></footer>
</main></body></html>
PHP
chown -R root:"$_fpmgrp" /var/www/app
chmod 0755 /var/www/app
chmod 0644 /var/www/app/*.php

# ─── nginx: php for the page and /healthz, a proxy for the operator's app ───
install -d -m 0755 /etc/nginx/conf.d
_server_name=_
[ -n "${WS_DOMAIN:-}" ] && _server_name="$WS_DOMAIN"
cat >/etc/nginx/conf.d/appstack.conf <<NGINX
server {
    listen 80 default_server;
    server_name ${_server_name};
    root /var/www/app;
    index index.php;
    location = /healthz { fastcgi_pass unix:${_fpmsock}; include fastcgi_params; fastcgi_param SCRIPT_FILENAME /var/www/app/healthz.php; }
    location /app/ {
        proxy_pass http://127.0.0.1:${WS_UPSTREAM_PORT}/;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
    location = /favicon.ico { access_log off; return 204; }
    location ~ \.php$ { fastcgi_pass unix:${_fpmsock}; include fastcgi_params; fastcgi_param SCRIPT_FILENAME \$document_root\$fastcgi_script_name; }
    location / { try_files \$uri \$uri/ =404; }
}
NGINX
# Debian ships a default site that also claims :80 default_server.
rm -f /etc/nginx/sites-enabled/default 2>/dev/null || true
# SELinux confines nginx and php-fpm (both httpd_t); without these booleans
# the proxy_pass is a 502 with a misleading log line and the page's own
# PostgreSQL and cache connections are refused.
# name=value pairs: "setsebool bool on bool on" is a usage error, and the
# first cut of this page shipped with both booleans off and every
# connection "Permission denied" (onyx, 2026-09-05). Not swallowed: a
# stack whose page cannot reach its stores is a failed recipe.
if command -v setsebool >/dev/null 2>&1 && selinuxenabled 2>/dev/null; then
    setsebool -P httpd_can_network_connect=on httpd_can_network_connect_db=on ||
        app_die "setsebool failed — php-fpm could not reach PostgreSQL or the cache under SELinux"
fi
# the checks below run in bash -c children; they need these
export _redcli WS_DB_NAME WS_DB_USER WS_DB_PASS WS_REDIS_PASS 2>/dev/null || true
app_selinux httpd_sys_content_t "/var/www/app(/.*)?"
app_relabel /var/www
app_enable "$_fpmsvc"
systemctl restart "$_fpmsvc" 2>/dev/null || true
nginx -t >/dev/null 2>&1 || { nginx -t; app_die "nginx config does not parse"; }
app_enable nginx
systemctl reload nginx 2>/dev/null || systemctl restart nginx

# HTTPS when a public name and contact are supplied. Best-effort: a LAN VM
# with no public DNS cannot pass the ACME challenge, and that must not fail
# the stack that works without it.
if [ -n "${WS_DOMAIN:-}" ] && [ -n "${WS_TLS_EMAIL:-}" ]; then
    if [ "$APP_FAMILY" = rpm ]; then app_pkg_optional certbot python3-certbot-nginx
    else app_pkg_optional certbot python3-certbot-nginx; fi
    if command -v certbot >/dev/null 2>&1; then
        certbot --nginx -n --agree-tos -m "$WS_TLS_EMAIL" -d "$WS_DOMAIN" 2>/dev/null ||
            app_warn "certbot could not issue for $WS_DOMAIN — HTTP stays up; re-run once DNS points here"
    fi
fi

# ─── firewall, verify ───────────────────────────────────────────────────────
app_firewall webstack "$WS_ALLOW_CIDR" 80/tcp 443/tcp

echo
app_check "postgres accepts SQL"   sudo -u postgres psql -tAc "SELECT 1"
app_check "database exists"        bash -c 'sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='"'"'$WS_DB_NAME'"'"'" | grep -q 1'
app_check "app user can connect"   bash -c 'PGPASSWORD="$WS_DB_PASS" psql -h 127.0.0.1 -U "$WS_DB_USER" -d "$WS_DB_NAME" -tAc "SELECT 1" | grep -q 1'
app_check "redis AUTH ping"        bash -c '"$_redcli" -a "$WS_REDIS_PASS" ping 2>/dev/null | grep -q PONG'
app_check "redis refuses no-auth"  bash -c '! "$_redcli" ping 2>/dev/null | grep -q PONG'
app_check "healthz: both stores answer" bash -c 'curl -fsS http://127.0.0.1/healthz | grep -q "\"ok\":true"'
app_check "page queries both"      bash -c 'curl -fsS http://127.0.0.1/ | grep -q "web stack up"'
app_check "page wrote a visit"     bash -c 'PGPASSWORD="$WS_DB_PASS" psql -h 127.0.0.1 -U "$WS_DB_USER" -d "$WS_DB_NAME" -tAc "SELECT count(*) FROM visits" | grep -qE "^[1-9]"'
app_check "postgres enabled"       systemctl is-enabled "$_pgsvc"
app_check "redis enabled"          systemctl is-enabled "$_redsvc"
app_check "php-fpm enabled"        systemctl is-enabled "$_fpmsvc"
app_check "nginx enabled"          systemctl is-enabled nginx
if [ -n "${APP_POOL:-}" ]; then
    app_check "pgdata recordsize 8K" bash -c '[ "$(zfs get -H -o value recordsize "$APP_POOL"/pgdata)" = 8K ]'
    app_snapshot postinstall-webstack
fi

cat <<EOM

  Web Stack

  Site        http://$(hostname -I 2>/dev/null | awk '{print $1}')/   (the example page: a visit row + a cache hit per load)
  Health      /healthz  — 200 only when PostgreSQL AND the cache answer
  App proxy   /app/ -> 127.0.0.1:${WS_UPSTREAM_PORT}
  Database    ${WS_DB_NAME} owner ${WS_DB_USER}  (127.0.0.1:5432, scram)
  Redis       127.0.0.1:6379 AUTH required
  Data        pool: ${APP_POOL:-<none — plain dirs>}
  Firewall    zone 'webstack', source ${WS_ALLOW_CIDR}

EOM
app_summary
`
