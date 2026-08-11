// validate.go — the form rules, testable without a display.
//
// These live OUTSIDE gui.go on purpose. gui.go is behind the `gui` build
// tag, so anything defined there is invisible to the static build and to
// `go test` — which is how a validator could have been written and never
// exercised. fyne.StringValidator is defined as func(string) error, so
// these assign straight onto a widget without importing Fyne here and
// dragging it into the static binary.
//
// WHY they exist: dialog.NewCustomConfirm cannot disable its own Confirm
// button, so an empty form was submittable and failed later — deep in the
// pipeline, after the dialog had closed and the operator had lost what they
// typed. Reported 2026-08-10: "it needed a vm name and still let me press
// go".
package main

import (
	"errors"
	"fmt"
	"strings"
)

// nameValidator enforces what libvirt and ZFS will both accept. A name with
// a space or a slash does not become an error later, it becomes a broken
// dataset path — so it is refused at the keyboard.
func nameValidator() func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return errors.New("a VM needs a name")
		}
		if !vmNameRE.MatchString(s) {
			return errors.New("letters, digits, then . _ - only")
		}
		return nil
	}
}

// numValidator checks a positive integer with a floor, naming the unit so
// the message reads as an instruction rather than a complaint.
//
// It rejects non-numeric input rather than letting it read as zero, which is
// what a bare fmt.Sscanf into an int did before: "two" vCPUs became 0 vCPUs
// and the failure surfaced as a libvirt error much further down.
func numValidator(min int, unit string) func(string) error {
	return func(s string) error {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err != nil {
			return errors.New("must be a number")
		}
		if v < min {
			return fmt.Errorf("at least %d %s", min, unit)
		}
		return nil
	}
}
