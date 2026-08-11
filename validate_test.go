package main

import "testing"

// The reported bug: an empty name sailed through and failed deep in the
// pipeline, after the dialog was gone and the typing was lost.
func TestEmptyNameIsRejected(t *testing.T) {
	v := nameValidator()
	for _, bad := range []string{"", "   ", "\t"} {
		if v(bad) == nil {
			t.Errorf("name %q must be rejected", bad)
		}
	}
	if err := v(""); err == nil || err.Error() != "a VM needs a name" {
		t.Errorf("the message should say what to do, got %v", err)
	}
}

// libvirt domain names and ZFS dataset names both constrain this; a name
// with a slash or a space becomes a broken dataset path, not an error.
func TestNameCharactersMatchTheSpecRule(t *testing.T) {
	v := nameValidator()
	for _, ok := range []string{"web01", "db-1", "a.b_c", "X9"} {
		if err := v(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"my vm", "a/b", "-lead", ".lead", "e$c"} {
		if v(bad) == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// A VM with 0 vCPU or 64MB is not a smaller VM, it is a broken one.
func TestNumericFloors(t *testing.T) {
	cpu := numValidator(1, "vcpu")
	if cpu("0") == nil || cpu("-2") == nil {
		t.Error("vcpu floor not enforced")
	}
	if cpu("2") != nil {
		t.Error("2 vcpus should be fine")
	}
	ram := numValidator(256, "MB of RAM")
	if ram("64") == nil {
		t.Error("64MB should be rejected")
	}
	if err := ram("64"); err == nil || err.Error() != "at least 256 MB of RAM" {
		t.Errorf("message should name the floor and unit, got %v", err)
	}
	// Non-numeric must not silently read as zero, which is what
	// fmt.Sscanf into an int did before.
	if numValidator(1, "vcpu")("two") == nil {
		t.Error("non-numeric input must be rejected, not parsed as 0")
	}
}

// The spec validator is the backstop the dialog now consults. Same rules.
func TestSpecValidateAgreesWithTheDialog(t *testing.T) {
	s := NewVMSpec{Name: "", VCPUs: 2, RAMMB: 2048, DiskGB: 20, Distro: "fedora"}
	if s.validate() == nil {
		t.Error("spec.validate must also reject an empty name")
	}
	s.Name = "ok1"
	s.VCPUs = 0
	if s.validate() == nil {
		t.Error("spec.validate must reject 0 vcpus")
	}
}
