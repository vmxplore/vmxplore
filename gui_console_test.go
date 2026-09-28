//go:build gui

package main

import (
	"testing"

	"fyne.io/fyne/v2"
)

// The quit chord is console mode's only way out under cage, so the default
// must parse and be one Fyne actually delivers; a bad VMX_QUIT_KEY falls back.
func TestQuitKey(t *testing.T) {
	t.Setenv("VMX_QUIT_KEY", "")
	sc, label := resolveQuitKey()
	if sc == nil || label != defaultQuitKey {
		t.Fatalf("default: %v %q", sc, label)
	}
	if err := chordDeliverable(sc); err != nil {
		t.Fatalf("default %s is not deliverable: %v", defaultQuitKey, err)
	}
	if sc.KeyName != fyne.KeyQ || sc.Modifier != fyne.KeyModifierControl|fyne.KeyModifierAlt {
		t.Fatalf("default parsed to %v %v", sc.KeyName, sc.Modifier)
	}
	t.Setenv("VMX_QUIT_KEY", "shift+q") // shift alone is never delivered
	if _, label = resolveQuitKey(); label != defaultQuitKey {
		t.Fatalf("an undeliverable VMX_QUIT_KEY was kept: %q", label)
	}
	t.Setenv("VMX_QUIT_KEY", "super+end")
	if _, label = resolveQuitKey(); label != "super+end" {
		t.Fatalf("a good VMX_QUIT_KEY was not used: %q", label)
	}
}
