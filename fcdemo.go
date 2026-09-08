// fcdemo.go — one touch, twelve machines, three windows.
//
// What it does, in order:
//  1. clones three goldens AT THE SAME TIME — four streamed desktops, four
//     RDP seats, four web servers — each `kfire clone <golden> -n 4 --wait`
//     in its own goroutine;
//  2. opens the VDI wall, every desktop on one page;
//  3. opens one RDP session per seat, ALREADY LOGGED IN;
//  4. opens the web servers in a browser.
//
// WHY: the estate's whole argument is "provision like a cloud, on your own
// iron", and the honest way to make that argument is to do it while someone
// watches. Twelve machines from three ZFS clones and three Firecracker
// batches lands in about a minute on a host that already holds the goldens,
// which is the length of the video this exists to record (operator,
// 2026-09-06). Serial, the same twelve took three times as long and read as
// a build rather than a deployment.
//
// The lanes run concurrently because they contend for nothing: three
// separate goldens, three separate zvol trees, three separate tap devices.
// kfire's own clone loop inside one lane stays sequential — that is where
// the 195 ms per clone is measured, and it is already fast.
//
// Inputs: kfire on the host with the three goldens present. Outputs: twelve
// running microVMs, a wall page in the browser, four RDP windows, four tabs.
// Nothing here writes to the estate that `kfire destroy --all` cannot undo.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DemoGuestUser and demoGuestPassword are the account every catalog tile is
// built with: `--build-all` calls Spec(vm, "admin", "", …) and cloud-init
// falls back to DefaultGuestPassword when the password is empty (newvm.go).
// A clone inherits the golden's /etc/shadow, so every seat in the batch
// takes the same login — which is what makes an unattended RDP session
// possible at all. An operator who built the tile with their own password
// overrides it with VMX_DEMO_PASSWORD; the seats then open on the login
// screen instead, which is a prompt, not a failure.
const DemoGuestUser = "admin"

func demoGuestPassword() string {
	if p := strings.TrimSpace(os.Getenv("VMX_DEMO_PASSWORD")); p != "" {
		return p
	}
	return DefaultGuestPassword
}

// demoLane is one row of the demo: a golden, how many, and what to open
// when they answer.
type demoLane struct {
	Golden string // kfire golden name
	Count  int
	Follow string // "wall" | "rdp" | "browser" — cloneFollowUp's vocabulary
	Label  string // what the log calls this lane
}

// DemoCount is how many of each kind the demo deploys. Four by default:
// enough that a wall of desktops reads as a wall and a rack of servers reads
// as a rack, few enough to fit in a minute and on a 16 GB host. A bigger
// host takes a bigger number — fiend has 62 GB and 24 cores and the operator
// wanted six of each (2026-09-06), which is eighteen machines.
//
// Measured, so the ceiling is not a guess: twelve microVMs took onyx from
// 10.0 GB free to 3.7 GB, about 525 MB apiece once ballooned. The cap is 32
// per lane — 96 machines, roughly 50 GB — because the operator asked for
// forty on a 62 GB host, and a limit that refuses a machine which can
// clearly do it is a limit in the wrong place. What actually stops a fleet
// is memory, so DemoFleetWarning checks that instead and says so.
const DemoCountDefault = 4

// DemoMachineMB is what one running microVM costs the host, measured rather
// than assumed (onyx, 2026-09-06: 12 instances took 6.3 GB).
const DemoMachineMB = 525

// demoCount reads $VMX_DEMO_COUNT, then /etc/vmxplore/demo-count, then falls
// back to the default.
//
// The FILE matters more than the variable: a GUI is started by the desktop
// session and inherits whatever that session had at login, so an environment
// file written afterwards reaches it only after a logout. fiend was set to
// fifteen of each and deployed four, because the running vmxplore had never
// seen the variable (2026-09-06). A file is read at the moment of the press.
func demoCount() int {
	v := strings.TrimSpace(os.Getenv("VMX_DEMO_COUNT"))
	if v == "" {
		if b, err := os.ReadFile("/etc/vmxplore/demo-count"); err == nil {
			v = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return DemoCountDefault
	}
	if n > 32 {
		return 32
	}
	return n
}

// DemoLanes is the fleet the one-touch tile deploys: demoCount() of each.
func DemoLanes() []demoLane {
	n := demoCount()
	return []demoLane{
		{Golden: "app-vdi-deskto", Count: n, Follow: "wall", Label: "streamed desktops"},
		{Golden: "app-rdp-deskto", Count: n, Follow: "rdp", Label: "RDP seats"},
		// LAMP, not the Web Stack: both serve a live page, but LAMP's counts
		// visits in MariaDB and prints the row count next to this instance's
		// own hostname and address, so four of them on screen are visibly
		// four machines with four databases rather than one page shown four
		// times (operator's call, 2026-09-06).
		{Golden: "app-lamp-stack", Count: n, Follow: "browser", Label: "LAMP servers"},
	}
}

// DemoMissingGoldens returns the lanes' goldens that `have` does not list,
// in lane order. The tile refuses to start on a missing golden rather than
// deploying two thirds of a demo: half a wall is worse than a clear message
// naming the one build that fixes it.
func DemoMissingGoldens(have []string) []string {
	present := make(map[string]bool, len(have))
	for _, g := range have {
		present[g] = true
	}
	var missing []string
	for _, l := range DemoLanes() {
		if !present[l.Golden] {
			missing = append(missing, l.Golden)
		}
	}
	return missing
}

// DemoFleetWarning reports whether the fleet plausibly fits in memory, and
// what to expect when it does not. Empty when there is room.
//
// A warning, never a refusal: the operator knows their host, the estimate is
// one measured number times a count, and a demo that refuses to start because
// a program did arithmetic is worse than one that starts and swaps. An
// availMB of 0 means "could not read it", and warns about nothing.
func DemoFleetWarning(machines, availMB int) string {
	if availMB <= 0 || machines <= 0 {
		return ""
	}
	need := machines * DemoMachineMB
	if need+2048 <= availMB {
		return ""
	}
	return fmt.Sprintf("%d machines want about %d MB and this host has %d MB free — expect swapping, or lower VMX_DEMO_COUNT",
		machines, need, availMB)
}

// hostAvailableMB is MemAvailable in MB, or 0 when it cannot be read.
func hostAvailableMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(f[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// DemoCloneArgv is the kfire command for one lane.
func DemoCloneArgv(l demoLane) []string {
	return kfireArgv("clone", l.Golden, "-n", fmt.Sprint(l.Count), "--wait")
}

// RunDemoLanes runs every lane's clone at once and waits for all of them.
// run is supplied by the caller — the GUI streams into its log window, the
// CLI writes to stderr — so this file stays free of both.
//
// Every lane is reported, and one lane's failure never cancels another: a
// demo with the web servers up and the desktops broken is still worth
// looking at, and the operator needs to know which half failed. Returns one
// error per failed lane, in lane order.
func RunDemoLanes(ctx context.Context, lanes []demoLane,
	run func(ctx context.Context, lane demoLane) error) []error {
	errs := make([]error, len(lanes))
	var wg sync.WaitGroup
	for i, l := range lanes {
		wg.Add(1)
		go func(i int, l demoLane) {
			defer wg.Done()
			if err := run(ctx, l); err != nil {
				errs[i] = fmt.Errorf("%s (%s): %w", l.Label, l.Golden, err)
			}
		}(i, l)
	}
	wg.Wait()
	out := errs[:0]
	for _, e := range errs {
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// BrowserArgv is the command that opens target — a URL or a local file.
//
// Firefox by name, first, on the operator's call (2026-09-06) and because
// the VDI wall needs it: the wall is a page of autoplaying WebRTC iframes,
// and Firefox plays them muted without a per-tile gesture where a
// Chrome-family default can sit on a black rectangle waiting for a click.
// xdg-open is the fallback, which honours whatever the desktop is set to.
// Empty when neither exists.
func BrowserArgv(target string) []string {
	for _, b := range []string{"firefox", "firefox-esr"} {
		if _, err := exec.LookPath(b); err == nil {
			return []string{b, target}
		}
	}
	if _, err := exec.LookPath("xdg-open"); err == nil {
		return []string{"xdg-open", target}
	}
	return nil
}

// RDPLoginArgv is the client that opens ONE seat already signed in, plus the
// password to write to its stdin.
//
// FreeRDP with /from-stdin:force, not /p:. A password in argv is a password
// in `ps` for every user on the machine; /from-stdin:force reads it from the
// pipe and silences the prompt, which is exactly an unattended login and
// exactly not a leak. Verified against FreeRDP 3.31 (onyx, 2026-09-06):
// /from-stdin alone reports "no password set" and prompts, the :force form
// consumes the piped line and proceeds to connect.
//
// Remmina is the fallback and CANNOT be pre-authenticated this way — it
// stores credentials in its own profile store — so it opens on the login
// screen, and the caller says so. Empty argv when neither client exists.
func RDPLoginArgv(ip, user string) (argv []string, wantsPasswordOnStdin bool) {
	for _, c := range []string{"xfreerdp3", "xfreerdp"} {
		if _, err := exec.LookPath(c); err == nil {
			return []string{c,
				"/v:" + ip,
				"/u:" + user,
				"/from-stdin:force",
				"/cert:ignore",
				"/dynamic-resolution",
				"/audio-mode:0",
				"+clipboard",
				"/t:" + user + "@" + ip, // window title, so four seats are tellable apart
			}, true
		}
	}
	if _, err := exec.LookPath("remmina"); err == nil {
		return []string{"remmina", "-c", "rdp://" + user + "@" + ip}, false
	}
	return nil, false
}

// DemoWebURL is the address of one web-server instance.
func DemoWebURL(ip string, port int) string {
	if port == 0 || port == 80 {
		return "http://" + ip + "/"
	}
	return fmt.Sprintf("http://%s:%d/", ip, port)
}

// OpenRDPSeats puts every seat on screen and returns how many opened plus a
// note about anything the operator should know.
//
// The wall first: one nested sway window with the seats tiled, which is what
// "a wall for RDP" means when the seats are native windows. Without sway
// (or without FreeRDP) it falls back to one window per seat and the note
// says which piece is missing — a demo that opens four windows is still a
// demo, and a silent downgrade would leave the operator wondering why the
// wall never came.
// seatSizeOrDefault is the /size: each RDP window opens at. A quarter of a
// 1080p screen by default, so four fit side by side once arranged: Wayland
// tells a client nothing about the display and GNOME places windows itself,
// so this is a sane size rather than a measurement. VMX_RDP_SIZE overrides.
func seatSizeOrDefault(env string) string {
	return seatSizeFor(env, DemoCountDefault)
}

// seatSizeFor is the window size for one of n seats. Four get a quarter of a
// 1080p screen each; a bigger batch gets smaller windows, because fourteen
// quarter-screen windows is a pile rather than a demo. VMX_RDP_SIZE
// overrides at any count.
func seatSizeFor(env string, n int) string {
	if s := strings.TrimSpace(env); s != "" {
		return s
	}
	switch {
	case n <= 4:
		return "960x540"
	case n <= 9:
		return "640x360"
	default:
		return "480x270"
	}
}

func OpenRDPSeats(ips []string) (opened int, note string) {
	if len(ips) == 0 {
		// Never silent: a lane that cloned four seats and found no address
		// has failed, and saying nothing is how it looks like it worked.
		return 0, "RDP: the seats have no address yet — open them from the estate"
	}
	pw := demoGuestPassword()
	size := seatSizeFor(os.Getenv("VMX_RDP_SIZE"), len(ips))
	type seat struct {
		ip  string
		cmd *exec.Cmd
	}
	live := make([]seat, 0, len(ips))
	for _, ip := range ips {
		argv, wantsStdin := RDPLoginArgv(ip, DemoGuestUser)
		if argv == nil {
			return opened, "no RDP client on this host (dnf install -y freerdp)"
		}
		argv = append(argv, "/size:"+size)
		cmd := exec.Command(argv[0], argv[1:]...)
		if wantsStdin {
			cmd.Stdin = strings.NewReader(pw + "\n")
		}
		var errBuf strings.Builder
		cmd.Stderr = &errBuf
		if err := cmd.Start(); err != nil {
			return opened, fmt.Sprintf("RDP: %s: %v", argv[0], err)
		}
		auditLog(strings.Join(argv, " "), 0)
		live = append(live, seat{ip: ip, cmd: cmd})
	}
	// Outcome, not exit code. Start() only says the binary launched; a
	// FreeRDP that cannot reach the guest, or is refused the login, is gone
	// within a second and the operator sees no window. Reporting the number
	// STARTED as the number opened is exactly how the sway wall reported
	// four seats while a black box sat on the screen (onyx, 2026-09-06).
	done := make(chan int, len(live))
	for i := range live {
		go func(i int) {
			_ = live[i].cmd.Wait()
			done <- i
		}(i)
	}
	titles := make([]string, 0, len(live))
	for _, sl := range live {
		titles = append(titles, DemoGuestUser+"@"+sl.ip)
	}
	dead := []string{}
	deadline := time.After(2500 * time.Millisecond)
	for range live {
		select {
		case i := <-done:
			dead = append(dead, live[i].ip)
		case <-deadline:
			// everything still running is a window on screen
			opened = len(live) - len(dead)
			tileNote := tileRDPSeats(titles)
			if len(dead) > 0 {
				return opened, fmt.Sprintf("RDP: %d seat(s) exited at once (%s) — check the guest is up and the login is %s",
					len(dead), strings.Join(dead, ", "), DemoGuestUser)
			}
			return opened, tileNote
		}
	}
	return 0, fmt.Sprintf("RDP: every seat exited immediately (%s) — check the guests are up and the login is %s",
		strings.Join(dead, ", "), DemoGuestUser)
}

// RunDemoCLI is `vmx --demo`: the tile's job from a script, for recording.
// Returns the number of lanes that failed, which is the process exit status.
func RunDemoCLI() int {
	say := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	gs, err := fcGoldens()
	if err != nil {
		say("vmx --demo: cannot list goldens — %v", err)
		return 1
	}
	have := make([]string, 0, len(gs))
	for _, g := range gs {
		have = append(have, g.Name)
	}
	if missing := DemoMissingGoldens(have); len(missing) > 0 {
		say("vmx --demo: this host has no golden for: %s", strings.Join(missing, ", "))
		say("            build the catalog first:  vmx --build-all")
		return len(missing)
	}
	lanes := DemoLanes()
	insts, err := fcInstances()
	if err != nil {
		say("vmx --demo: cannot list instances — %v", err)
		return 1
	}
	before := fcInstanceNames(fcRows(insts))
	start := time.Now()
	machinesPlanned := 0
	for _, l := range lanes {
		machinesPlanned += l.Count
	}
	say("deploying the demo estate — %d machines, %d lanes at once", machinesPlanned, len(lanes))
	if w := DemoFleetWarning(machinesPlanned, hostAvailableMB()); w != "" {
		say("WARNING: %s", w)
	}
	var mu sync.Mutex
	machines := 0
	// The wall is written from whatever kfire reports at the moment the VDI
	// lane lands, which on the CLI is the only estate there is.
	openWall := func() {
		streams := VDIWallStreams(fcRowsCached(), whepProbe)
		path, werr := WriteVDIWall(streams)
		if werr != nil {
			say("   VDI wall: %v", werr)
			return
		}
		if argv := BrowserArgv("file://" + path); argv != nil {
			if err := exec.Command(argv[0], argv[1:]...).Start(); err != nil {
				say("   VDI wall: %v", err)
				return
			}
			say("   VDI wall: %d desktop(s) — %s", len(streams), path)
		}
	}
	errs := RunDemoLanes(context.Background(), lanes, func(ctx context.Context, l demoLane) error {
		say("── %s: %d × %s", l.Label, l.Count, l.Golden)
		argv := DemoCloneArgv(l)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		fcInvalidate()
		fresh := DemoFreshWithAddresses(l.Golden, before, l.Count,
			fcRowsCached, fcInvalidate, 45*time.Second)
		mu.Lock()
		machines += len(fresh)
		mu.Unlock()
		if len(fresh) == 0 {
			return nil
		}
		_, note := openDemoSurface(l, fresh, openWall, func(s string) { say("%s", s) })
		if note != "" {
			say("   %s", note)
		}
		say("   %s: on screen after %s", l.Label, time.Since(start).Round(time.Second))
		return nil
	})
	for _, e := range errs {
		say("FAILED: %v", e)
	}
	say("── %d machines in %s", machines, time.Since(start).Round(time.Second))
	return len(errs)
}

// openDemoSurface puts ONE lane's fresh machines on screen: the wall for
// streamed desktops, the tiled seats for RDP, Firefox for anything serving a
// page. Returns how many were opened and a note about any degradation.
//
// It is called from inside the lane's own goroutine, the moment that lane's
// clones answer — the desktops are up in seconds and the web tiles take about
// a minute, and making the fast ones wait for the slow one is the difference
// between a demo that unfolds and a demo that sits on a log for a minute.
//
// openWall is the caller's — the GUI has one that reuses the window's rows,
// the CLI passes its own — so this file needs neither Fyne nor the estate.
func openDemoSurface(l demoLane, fresh []Row, openWall func(), log func(string)) (opened int, note string) {
	switch l.Follow {
	case "wall":
		if openWall != nil {
			openWall()
		}
		log(fmt.Sprintf("   VDI wall: %d desktop(s)", len(fresh)))
		return len(fresh), ""
	case "rdp":
		ips := make([]string, 0, len(fresh))
		for _, r := range fresh {
			if ip := firstIPv4(r.D.IPs); ip != "" {
				ips = append(ips, ip)
			}
		}
		n, note := OpenRDPSeats(ips)
		log(fmt.Sprintf("   RDP: %d seat(s) as %s", n, DemoGuestUser))
		return n, note
	case "browser":
		urls := make([]string, 0, len(fresh))
		for _, r := range fresh {
			if ip := firstIPv4(r.D.IPs); ip != "" {
				urls = append(urls, DemoWebURL(ip, r.FC.Port))
			}
		}
		if len(urls) == 0 {
			return 0, l.Label + ": no addresses yet"
		}
		argv := BrowserArgv(urls[0])
		if argv == nil {
			return 0, "no browser on this host (firefox or xdg-open)"
		}
		// One Firefox invocation with every URL: four separate ones race to
		// own the profile and the later ones can exit without a window.
		if err := exec.Command(argv[0], append(argv[1:], urls[1:]...)...).Start(); err != nil {
			return 0, "browser: " + err.Error()
		}
		auditLog(strings.Join(argv, " ")+fmt.Sprintf(" (+%d more)", len(urls)-1), 0)
		log(fmt.Sprintf("   web: %d tab(s) in %s", len(urls), filepath.Base(argv[0])))
		return len(urls), ""
	}
	return 0, ""
}

// tileRDPSeats arranges the seat windows into a grid, best effort.
//
// WHY THIS CAN WORK AT ALL: FreeRDP's X11 client runs under XWayland even on
// a Wayland desktop, so its windows ARE X11 windows and wmctrl can place
// them — which a Wayland-native client could never allow. vmxplore runs
// inside the session and has DISPLAY and XAUTHORITY, which is exactly what an
// ssh shell does not (that is why the same test failed from outside,
// 2026-09-06). Without wmctrl, or on a compositor that refuses the move, the
// windows simply stay where the desktop put them and the demo is unharmed.
//
// titles are the /t: values the seats were given, which is how each window is
// found without guessing at process ids.
func tileRDPSeats(titles []string) string {
	wmctrl, err := exec.LookPath("wmctrl")
	if err != nil {
		return "windows not tiled — wmctrl is not installed (dnf install -y wmctrl)"
	}
	if os.Getenv("DISPLAY") == "" {
		return "windows not tiled — no X display in this session"
	}
	cols := 2
	for cols*cols < len(titles) {
		cols++
	}
	rows := (len(titles) + cols - 1) / cols
	// A 1920x1080 work area is the assumption when nothing better is known;
	// wmctrl reports the real one where it can.
	sw, sh := 1920, 1053
	if out, err := exec.Command(wmctrl, "-d").Output(); err == nil {
		for _, f := range strings.Fields(string(out)) {
			if x, y, ok := strings.Cut(f, "x"); ok {
				if a, e1 := strconv.Atoi(x); e1 == nil {
					if b, e2 := strconv.Atoi(y); e2 == nil && a > 640 && b > 480 {
						sw, sh = a, b-27
						break
					}
				}
			}
		}
	}
	w, h := sw/cols, sh/rows
	placed := 0
	for i, t := range titles {
		x, y := (i%cols)*w, (i/cols)*h
		geom := fmt.Sprintf("0,%d,%d,%d,%d", x, y, w, h)
		if err := exec.Command(wmctrl, "-r", t, "-e", geom).Run(); err == nil {
			placed++
		}
	}
	if placed == 0 {
		return "windows not tiled — the compositor refused every move"
	}
	if placed < len(titles) {
		return fmt.Sprintf("tiled %d of %d windows", placed, len(titles))
	}
	return ""
}

// DemoFreshWithAddresses returns this lane's new instances once they have
// addresses, waiting up to wait for them.
//
// A clone answers its port before the estate necessarily knows its address:
// kfire writes the address into the instance record as it enrolls, and the
// row cache is read a moment later. At four per lane that gap never showed;
// at fifteen, under a host running fifteen video encoders, the LAMP lane
// asked for addresses, got none, and opened no browser at all — the tile
// reported "no addresses yet" and the operator saw no tabs (fiend,
// 2026-09-06). Waiting a few seconds is the difference between a demo that
// opens and a demo that explains why it did not.
//
// rows is the caller's row source so this stays out of the estate's caching;
// invalidate is what makes the next call ask kfire again.
func DemoFreshWithAddresses(golden string, before map[string]bool, want int,
	rows func() []Row, invalidate func(), wait time.Duration) []Row {
	deadline := time.Now().Add(wait)
	var fresh []Row
	for {
		fresh = newInstancesOf(golden, before, rows())
		withIP := 0
		for _, r := range fresh {
			if firstIPv4(r.D.IPs) != "" {
				withIP++
			}
		}
		if withIP >= want || time.Now().After(deadline) {
			return fresh
		}
		time.Sleep(time.Second)
		if invalidate != nil {
			invalidate()
		}
	}
}

// DemoBlockers reports why the estate is not ready to deploy, or nothing when
// it is. Checked before the tile starts rather than after it fails.
//
// fiend, 2026-09-06: destroy-all was pressed twice and the demo tile pressed
// while the second teardown was still settling. All three lanes returned an
// error, the log window filled with them, and the operator was left with a
// window that would not close and an inventory full of half-registered
// machines. kfire was healthy the whole time — a single clone straight
// afterwards took 252 ms. The fault was starting on top of a teardown.
//
// live is how many microVM records still exist; goldens is what the host has.
// Both come from the caller so this stays testable without kfire.
func DemoBlockers(tearingDown bool, live int, goldens []string) []string {
	var why []string
	if tearingDown {
		why = append(why, "a teardown is still running — wait for it to finish, then press Deploy again")
	}
	if live > 0 {
		why = append(why, fmt.Sprintf("%d microVM(s) are still here — \"Destroy all microVMs\" first, so the fleet you deploy is the fleet you see", live))
	}
	if missing := DemoMissingGoldens(goldens); len(missing) > 0 {
		why = append(why, "no golden for: "+strings.Join(missing, ", ")+" — build the catalog first")
	}
	return why
}

// demoTeardownRunning reports whether a kfire destroy is in flight. pgrep is
// the probe because the teardown is a child process, not a state file.
//
// The pattern is ANCHORED at the start of the command line. A bare
// `pgrep -f "kfire destroy"` matches any process whose arguments merely
// contain that text — a shell one-liner about it, an editor with the script
// open, or the very test that was checking the probe (caught 2026-09-06).
// A false positive here blocks the demo for no reason, which is worse than
// the race it exists to prevent.
func demoTeardownRunning() bool {
	const pat = `^(sudo( +-[A-Za-z]+)* +)?[^ ]*kfire +(destroy|golden-destroy)\b`
	out, err := exec.Command("pgrep", "-f", pat).Output()
	if err != nil {
		// pgrep exits 1 when nothing matches, which is the common case and
		// not an error worth reporting.
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// demoLaneBreakdown renders the per-lane counts as one line — "15 streamed
// desktops   15 RDP seats   15 LAMP servers".
//
// It exists because the numbers used to be typed into the dialog by hand while
// the total beside them was computed. Once the fleet size became a knob the two
// disagreed, and the take recorded on fiend 2026-09-07 has "4 streamed desktops"
// sitting directly above "15 × app-vdi-deskto" in the same window. Anything the
// operator reads on screen during a demo has to come from the same source as
// the thing being demonstrated.
func demoLaneBreakdown(lanes []demoLane) string {
	parts := make([]string, 0, len(lanes))
	for _, l := range lanes {
		parts = append(parts, fmt.Sprintf("%d %s", l.Count, l.Label))
	}
	return strings.Join(parts, "   ")
}

// demoTileDetail is the one-line subtitle under the demo tile in the estate
// list. Same rule as demoLaneBreakdown: derived, never typed.
func demoTileDetail() string {
	lanes := DemoLanes()
	total := 0
	for _, l := range lanes {
		total += l.Count
	}
	return fmt.Sprintf("%d machines at once — %s — then the walls open",
		total, demoLaneBreakdown(lanes))
}
