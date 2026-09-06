// vdiwall.go — the all-seeing eye: every VDI desktop on the estate, on one page.
//
// What it does, in order:
//  1. picks the rows that are VDI desktops — the app-vdi appliance VM, any
//     Firecracker microVM cloned from its golden, any domain with "vdi" in
//     its name — and takes each one's first IPv4;
//  2. asks each host which sessions it is streaming, by knocking on each
//     session's WHEP endpoint: mediamtx answers a live path with 400
//     (it wants a real SDP) and a missing one with 404, so the probe walks
//     1, 2, 3… and stops at the first 404. The player PAGE is no use for
//     this — it is static and answers 200 for any name, which is how the
//     first cut listed 32 sessions on a host with one (onyx, 2026-09-05);
//  3. writes one HTML page that tiles every stream as an iframe on the
//     WebRTC player, muted and autoplaying, with the host and session under
//     each, and returns its path for a browser to open.
//
// WHY: one VDI VM was a URL to paste; ten clones are ten URLs, and the
// operator asked to "see all 4" and then for "an all-seeing eye" (2026-09-05).
// The page is static and needs nothing on the host but a browser; every
// pixel comes straight from each guest's own mediamtx, so the wall scales
// with the guests, not with vmxplore.
//
// Notes: WebRTC in an iframe needs the allow="autoplay" attribute or the
// browser blocks playback silently; the player is opened muted for the same
// reason. The page is written under os.TempDir so both the CLI and the GUI
// hand the same file to the browser.
package main

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// wallStream is one desktop on the wall.
type wallStream struct {
	Host    string // VM or microVM name
	IP      string
	Session int
	Player  string // the WebRTC page, muted + autoplay, for the iframe
	Page    string // the same page with controls, for a click-through
}

// vdiWallPort is mediamtx's WebRTC port in the VDI recipe (homelab.go).
const vdiWallPort = 8889

// isVDIRow says whether a row is a VDI desktop: the appliance VM itself
// (app-vdi-…), a microVM cloned from its golden, or anything an operator
// named with "vdi" in it. Name-based on purpose: the state DB is not on a
// plain KVM host, and the name is what every path here has.
func isVDIRow(r Row) bool {
	if r.FC != nil {
		return strings.HasPrefix(r.FC.Golden, "app-vdi")
	}
	n := strings.ToLower(r.D.Name)
	return strings.HasPrefix(n, "app-vdi") || strings.Contains(n, "vdi")
}

// vdiSessionsAt walks /session1/, /session2/… on one host until the first
// miss. Sessions are contiguous by construction (vdi-session@N units from
// 1 to VDI_SESSIONS), so the first gap is the end. Capped so a host that
// answers 200 to anything cannot make this loop forever.
func vdiSessionsAt(ip string, probe func(url string) bool) []int {
	var out []int
	for n := 1; n <= 32; n++ {
		if !probe(fmt.Sprintf("http://%s:%d/session%d/", ip, vdiWallPort, n)) {
			break
		}
		out = append(out, n)
	}
	return out
}

// whepProbe is the live probe. base is the session URL ("…/sessionN/");
// a WHEP POST with an empty SDP makes mediamtx say whether the path has a
// stream: 400 "failed to unmarshal SDP" means yes, 404 "no stream is
// available" means no. 201 would mean it accepted the offer, which an
// empty SDP never earns, but counts as live too.
func whepProbe(base string) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Post(base+"whep", "application/sdp", strings.NewReader("v=0"))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusCreated
}

// VDIWallStreams finds every VDI stream among rows. probe is injectable so
// the selection and the page can be tested without a guest.
func VDIWallStreams(rows []Row, probe func(string) bool) []wallStream {
	var out []wallStream
	seen := map[string]bool{}
	for _, r := range rows {
		if !isVDIRow(r) || r.D.State != "running" {
			continue
		}
		ip := firstIPv4(r.D.IPs)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		for _, n := range vdiSessionsAt(ip, probe) {
			base := fmt.Sprintf("http://%s:%d/session%d/", ip, vdiWallPort, n)
			out = append(out, wallStream{
				Host:    r.D.Name,
				IP:      ip,
				Session: n,
				Player:  base + "?autoplay=true&muted=true&controls=false",
				Page:    base,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Session < out[j].Session
	})
	return out
}

// VDIWallHTML renders the wall. Columns follow the count so four desktops
// are a 2×2 and ten are a 4×3, each tile 16:9; a tile's caption is a link to
// the full player with controls.
func VDIWallHTML(streams []wallStream) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>VDI wall</title>
<style>
:root{--bg:#0b0d12;--ink:#e8eaf0;--mut:#8b93a7;--line:#1f2431;--acc:#c77dff}
*{box-sizing:border-box}html,body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.4 system-ui,sans-serif;height:100%}
header{display:flex;align-items:baseline;gap:14px;padding:12px 18px;border-bottom:1px solid var(--line)}
header h1{margin:0;font-size:16px;font-weight:600;letter-spacing:.02em}header h1 b{color:var(--acc)}
header span{color:var(--mut)}header a{margin-left:auto;color:var(--mut);text-decoration:none}header a:hover{color:var(--ink)}
main{display:grid;gap:10px;padding:10px 18px 18px;grid-template-columns:repeat(var(--cols),1fr)}
figure{margin:0;background:#000;border:1px solid var(--line);border-radius:8px;overflow:hidden;display:flex;flex-direction:column}
.shot{position:relative;aspect-ratio:16/9;max-height:calc(100vh - 120px);background:#000}.shot iframe{position:absolute;inset:0;width:100%;height:100%;border:0}
figcaption{display:flex;gap:10px;align-items:baseline;padding:6px 10px;background:#10131a;font-size:12.5px}
figcaption b{font-weight:600}figcaption span{color:var(--mut);font-family:ui-monospace,monospace;font-size:11.5px}
figcaption a{margin-left:auto;color:var(--acc);text-decoration:none}figcaption a:hover{text-decoration:underline}
.empty{padding:60px 18px;color:var(--mut);text-align:center}
</style></head><body>
`)
	cols := 1
	for cols*cols < len(streams) {
		cols++
	}
	if cols > 4 {
		cols = 4
	}
	fmt.Fprintf(&b, `<header><h1>VDI wall <b>·</b> %d desktop%s</h1><span>every VDI stream on the estate, live — muted; click a name for sound and controls</span><a href="javascript:location.reload()">reload</a></header>
<main style="--cols:%d">
`, len(streams), plural(len(streams)), cols)
	if len(streams) == 0 {
		b.WriteString(`<div class="empty">No VDI desktop is streaming. Start the VDI appliance, or clone its Firecracker golden, and reload.</div>`)
	}
	for _, s := range streams {
		fmt.Fprintf(&b, `<figure><div class="shot"><iframe src="%s" allow="autoplay" loading="eager" title="%s session %d"></iframe></div><figcaption><b>%s</b><span>%s · session%d</span><a href="%s" target="_blank">open ↗</a></figcaption></figure>
`, html.EscapeString(s.Player), html.EscapeString(s.Host), s.Session, html.EscapeString(s.Host), html.EscapeString(s.IP), s.Session, html.EscapeString(s.Page))
	}
	b.WriteString("</main></body></html>\n")
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// WriteVDIWall writes the page and returns its path.
func WriteVDIWall(streams []wallStream) (string, error) {
	p := filepath.Join(os.TempDir(), "vmx-vdi-wall.html")
	if err := os.WriteFile(p, []byte(VDIWallHTML(streams)), 0o644); err != nil {
		return "", fmt.Errorf("writing the wall page: %w", err)
	}
	return p, nil
}

// wallRowsFromLibvirt is the CLI's view of the estate: every domain as a
// row, plus the Firecracker instances. The GUI passes its own rows.
func wallRowsFromLibvirt() ([]Row, error) {
	lv, err := ConnectSystem()
	if err != nil {
		return nil, err
	}
	defer lv.Close()
	doms, err := lv.Estate()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(doms))
	for _, d := range doms {
		rows = append(rows, Row{D: d})
	}
	return append(rows, fcRowsCached()...), nil
}
