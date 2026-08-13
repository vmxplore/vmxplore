// firmware.go — the guest's firmware and its TPM.
//
// WHAT IT DOES: turns two booleans on a NewVMSpec into virt-install
// arguments, and nothing else. No probing, no autodetection.
//
// WHY IT EXISTS: vmxplore's New VM path had no firmware handling at all, so
// every guest it built was SeaBIOS with no TPM. That is fine for Linux and
// fine for Windows 10, and it makes Windows 11 impossible — Microsoft
// requires UEFI, Secure Boot capability and TPM 2.0, and a guest without them
// fails setup's compatibility check before the install begins. The kldload
// toolset covers Windows through kvm-win, but someone running vmxplore on
// stock Linux had no route at all and no explanation.
//
// UEFI is worth offering beyond Windows: it is the normal firmware for modern
// Linux guests, and a guest installed under SeaBIOS cannot be switched later
// without reinstalling.
//
// NOTES:
//   - The host needs OVMF for UEFI and swtpm for the TPM. Both ship in every
//     distribution's virtualisation group, and virt-install fails loudly and
//     specifically when they are missing — which is a better error than
//     anything this could invent by probing for files.
//   - tpm-crb rather than tpm-tis: CRB is what modern Windows expects, and
//     Linux drives either.
package main

// firmwareArgs returns the virt-install arguments for a spec's firmware.
//
// Args:    s — the spec.
// Returns: the arguments, possibly empty. Empty means SeaBIOS with no TPM,
//
//	which is virt-install's own default and the historical behaviour.
func firmwareArgs(s NewVMSpec) []string {
	var a []string
	if s.UEFI {
		a = append(a, "--boot", "uefi")
	}
	if s.TPM {
		a = append(a, "--tpm",
			"backend.type=emulator,backend.version=2.0,model=tpm-crb")
	}
	return a
}
