// vdiwall_session_test.go — the wall's --open says why it cannot open a
// browser instead of handing the page to an xdg-open that opens nothing.
package main

import (
	"strings"
	"testing"
)

func TestDesktopSession(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cases := []struct {
		name string
		euid int
		env  map[string]string
		ok   bool
		why  string
	}{
		{"root under sudo", 0, map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, false, "root"},
		{"ssh, no display", 1000, map[string]string{}, false, "no desktop session"},
		{"wayland session", 1000, map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, true, ""},
		{"x11 session", 1000, map[string]string{"DISPLAY": ":0"}, true, ""},
	}
	for _, c := range cases {
		ok, why := desktopSession(c.euid, env(c.env))
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: got (%v, %q), want ok=%v and why containing %q", c.name, ok, why, c.ok, c.why)
		}
	}
}
