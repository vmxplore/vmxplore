// kld — the kldload operator console, in a terminal.
//
// One binary, the same eight sections as the web console on :8443, read from
// the same tools: kldload-estate, kldload-doctor, zpool, kldload-rollback,
// wg, kubectl, Prometheus and kldload-netboot-server. It is a hub, not a
// fifth console: the deep views stay in vmxplore, zxplore and wgx, and this
// tool opens them on Enter and comes back when they exit.
//
//	kld                      the terminal console
//	kld <section> [sub]      open on that section and sub-tab
//	kld --gui [section]      this console in a terminal window (the GUI is the TUI)
//	kld <section> --print    print that sub-tab once and exit (no TUI)
//	kld --version
//
// One app for both a window manager and a headless box (operator,
// 2026-09-26): the terminal console is the console, everywhere, and is the
// default even under a display — the operator typed `kld` on a desktop and
// got a Chrome window when they wanted the TUI (10:20 that day). --gui opens
// this console in a terminal window through kldload-term: the GUI is the
// TUI, so nothing drifts between them.
//
// WHY: on 2026-09-26 the estate had four terminal consoles with four key
// maps, and the answers an operator wants first — what is running, what
// drifted, what boots next, who is on the mesh, is the rack armed — lived in
// eight different commands. The web console got one sidebar by task; this
// is the same sidebar for ssh.
//
// Verbs here go through the shipped bash verbs (kvm-clone, kvm-delete,
// kvm-snap, kldload-netboot-server) so the TUI cannot drift from what the
// tests prove; nothing is re-implemented.
//
// Exit: 0; 2 on a usage error.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

const version = "0.1.0"

// buildNum is stamped by the Makefile / build-iso.sh (-X main.buildNum=…),
// the same way wgx and zxplore carry theirs.
var buildNum = ""

func versionFull() string {
	if buildNum == "" || buildNum == "0" {
		return version
	}
	return version + " b" + buildNum
}

func usage() {
	fmt.Print(`vmx ` + versionFull() + ` — the vmxplore terminal console

  vmx                       open the terminal console
  vmx <section> [<sub-tab>] open on a section: overview machines storage
                            network cluster ansible helm metrics estate
                            provision — and one of its sub-tabs
  vmx --gui [section]       this console in a window: vmxplore's terminal
                            (vmxplore --term) when the GUI is installed,
                            else kldload-term; needs a display
  vmx <section> [<sub-tab>] --print
                            print that sub-tab once and exit
  vmx screen <vm> [--blocks|--sixel]
                            the VM's display full-screen in this terminal:
                            sixels where the terminal draws them (never under
                            tmux unless --sixel), half-block cells otherwise
                            (--blocks forces them); ctrl+] is the menu, d
                            detaches
  vmx install               the install menu: profile, distribution, security,
                            disk, hostname, user — then the unattended
                            installer; or provision another machine
  vmx --version

Keys inside:  1-9, 0  section   tab  sub-tab   j/k  row   enter  drill in
(a VM's or a dataset's snapshots)   /  filter   o  sort   i  vitals pane
r  reload   ?  the verbs of the current tab   q  quit
Verbs run the shipped commands (virsh, kvm-*, zfs, kldload-rollback,
kldload-enroll, kubectl, ansible, helm, kldload-netboot-server); the
destructive ones ask for the name to be typed back.

The non-interactive jobs (--build-all, --selftest, --appliances,
--vdi-wall, ...) are vmxctl's; the GUI is vmxplore. For one release vmx
still forwards those flags to vmxctl, with a warning.
`)
}

func main() {
	args := os.Args[1:]
	forwardOldFlags(args)
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			usage()
			return
		case "--version", "-V":
			fmt.Println("vmx " + versionFull())
			return
		case "install":
			// the install menu: the live medium's boot target
			if err := runInstallMenu(); err != nil {
				fmt.Fprintln(os.Stderr, "vmx install:", err)
				os.Exit(1)
			}
			return
		case "screen":
			// a VM's display, full-screen in this terminal (screen.go)
			if err := runScreen(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "vmx screen:", err)
				os.Exit(1)
			}
			return
		}
	}
	start, sub := 0, 0
	print, gui, sawSection := false, false, false
	for _, a := range args {
		switch a {
		case "--print":
			print = true
			continue
		case "--gui":
			gui = true
			continue
		case "--tui": // the old default's opt-out, kept as a no-op alias
			continue
		}
		// a sub-tab of the section already named wins over a section of the
		// same name: `kld metrics machines` is Metrics/Machines, not Machines
		if sawSection {
			if j := subIndex(start, a); j >= 0 {
				sub = j
				continue
			}
		}
		if i := sectionIndex(a); i >= 0 {
			start, sub, sawSection = i, 0, true
			continue
		}
		if j := subIndex(start, a); j >= 0 {
			sub = j
			continue
		}
		fmt.Fprintf(os.Stderr, "vmx: no section or sub-tab named %q (sections: %s; sub-tabs of %s: %s)\n",
			a, strings.Join(sectionNames(), " "), sections[start].name, strings.ToLower(strings.Join(sections[start].subs, " ")))
		os.Exit(2)
	}
	if print {
		// One sub-tab, rendered once, for scripts and for testing the
		// renderers without a terminal: body() is pure, so what --print
		// shows is what the TUI shows.
		m := newModel(start, sub, 120)
		m.apply(loadSection(start, sub, ""))
		fmt.Print(m.body())
		return
	}
	if gui && !print {
		if err := openWindow(start); err != nil {
			fmt.Fprintln(os.Stderr, "vmx:", err, "— starting the terminal console")
		} else {
			return
		}
	}
	sixelTerminal = terminalHasSixel()
	if _, err := tea.NewProgram(newModel(start, sub, 0), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "vmx:", err)
		os.Exit(1)
	}
}

// ── the window ──────────────────────────────────────────────────────────────

var errNoDisplay = errors.New("no display")

// openWindow puts this console in a window of its own: vmxplore's terminal
// (`vmxplore --term vmx --tui SECTION`) when the GUI build is installed --
// the terminal the operator chose, and the same thing the desktop icon runs
// (2026-09-30) -- else kldload-term --app, the wrapper every terminal
// launcher on a kldload host uses. One program for a window manager and a
// headless box (operator, 2026-09-26: "no web gui at all"). It returns
// errNoDisplay when there is nothing to draw on, and any other error when
// neither window is available, so the caller falls back to the terminal it
// is already in.
func openWindow(section int) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errNoDisplay
	}
	self := selfExe()
	sec := strings.ToLower(sections[section].name)
	var c *exec.Cmd
	if gui, err := exec.LookPath("vmxplore"); err == nil {
		c = exec.Command(gui, "--term", self, "--tui", sec)
	} else if wrapper, err := exec.LookPath("kldload-term"); err == nil {
		c = exec.Command(wrapper, "--app", self, "--tui", sec)
	} else {
		return errors.New("neither vmxplore nor kldload-term is installed")
	}
	c.Stdin = os.Stdin
	return c.Run()
}

// selfExe is this binary's own path, for the verbs that run vmx again (the
// full-window screen). By name it was "kld", which a host with vmx and no kld
// does not have; os.Executable is right whatever the binary is called.
func selfExe() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "vmx"
}

// forwardOldFlags hands the old `vmx` job flags to the binary that does them
// now, with a warning, and does not return when it does. Until 2026-09-30
// `vmx` was the bubbletea TUI AND every non-interactive job (--build-all,
// --selftest, --appliances ...); the jobs moved to vmxctl and the GUI-only
// --console to vmxplore, so vmx is only the console. kldload's first boot and
// people's habits still type `vmx --build-all`: for one release that keeps
// working, says where it went, and then this goes (project rule: a renamed
// interface keeps a warning alias for a release).
func forwardOldFlags(args []string) {
	if len(args) == 0 {
		return
	}
	target := ""
	switch args[0] {
	case "--console":
		target = "vmxplore"
	case "--once", "--reconcile", "--orphans", "--setup", "--selftest", "--demo",
		"--build-all", "--enroll", "--destroy-all", "--appliances", "--sysdiag",
		"--vdi-wall", "--appliance", "--appliance-script", "--connect", "-c",
		"--rules", "-r":
		target = "vmxctl"
	default:
		return
	}
	fmt.Fprintf(os.Stderr, "vmx: %s moved to %s; running `%s %s` (this forward goes in a later release)\n",
		args[0], target, target, strings.Join(args, " "))
	path, err := exec.LookPath(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmx: %s is not installed, so %s cannot run\n", target, args[0])
		os.Exit(127)
	}
	if err := syscall.Exec(path, append([]string{target}, args...), os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "vmx: running %s: %v\n", target, err)
		os.Exit(1)
	}
}
