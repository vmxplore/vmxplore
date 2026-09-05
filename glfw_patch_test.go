// glfw_patch_test.go — the vendored GLFW carries the one change this app
// depends on for Wayland fullscreen; a refresh that forgets it builds and
// runs and jumps monitors again. See third_party/glfw/VMXPLORE-PATCH.md.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestGLFWPatchPresent(t *testing.T) {
	src, err := os.ReadFile("third_party/glfw/glfw/src/wl_window.c")
	if err != nil {
		t.Fatalf("vendored GLFW missing: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "vmxplore patch") ||
		!strings.Contains(s, "xdg_toplevel_set_fullscreen(window->wl.xdg.toplevel, NULL)") ||
		!strings.Contains(s, "libdecor_frame_set_fullscreen(window->wl.libdecor.frame, NULL)") {
		t.Fatal("wl_window.c acquireMonitor() does not pass a NULL output — the fullscreen patch is gone")
	}
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "replace github.com/go-gl/glfw/v3.4/glfw => ./third_party/glfw") {
		t.Fatal("go.mod no longer replaces GLFW with the patched copy")
	}
}
