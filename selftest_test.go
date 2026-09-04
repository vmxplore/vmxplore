package main

import "testing"

// Every tile must map to a distinct VM name for BOTH naming schemes, and each
// name must survive the mesh derivation, whose 15-character cap is the
// kernel's. A collision here would make build-all skip a tile as "already
// exists" and destroy-all tear down the wrong one.
func TestApplianceVMNamesDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, a := range Appliances() {
		for _, vm := range []string{applianceVMName(a.Name), selfTestVM(a.Name)} {
			if prev, dup := seen[vm]; dup {
				t.Errorf("%q and %q both map to VM %q", prev, a.Name, vm)
			}
			seen[vm] = a.Name
			if m := enrollMeshName(vm); len(m) > 15 || len(m) < 4 {
				t.Errorf("mesh name for %q is %q", vm, m)
			}
		}
	}
	if len(seen) != 2*len(Appliances()) {
		t.Errorf("%d names for %d tiles", len(seen), len(Appliances()))
	}
}
