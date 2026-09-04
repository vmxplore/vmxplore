// usb_attach.go — pass host USB devices through to an appliance guest.
//
// WHY: the SDR and DVR tiles are built around hardware — a HackRF, an RTL
// stick, a USB tuner. The build can finish without them, but the appliance is
// then a web page with no antenna. Attachment is live+persistent: the guest
// sees a hotplug now AND finds the device again after a reboot.
//
// PCI capture cards are deliberately out of scope here: vfio/IOMMU passthrough
// is host configuration, not something to spring on a machine mid-build. The
// tiles' Notes say so and name the manual path.
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var usbIDRE = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{4}$`)

// AttachUSBDevices attaches each present vendor:product to the running guest.
// Absent devices warn and are skipped — best-effort by design, like
// enrollment: a missing radio must not destroy a finished build.
func AttachUSBDevices(vmName string, ids []string, log func(string)) {
	if len(ids) == 0 {
		return
	}
	if target.SSHHost != "" {
		log("usb: passthrough is local-host only for now — skipped on a remote target")
		return
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !usbIDRE.MatchString(id) {
			log("usb: " + id + " is not vendor:product hex — skipped")
			continue
		}
		// Present on the host? lsusb -d exits 0 with output only on a match.
		if out, err := sudoRun("lsusb", "-d", id); err != nil || strings.TrimSpace(out) == "" {
			log("usb: " + id + " not present on the host — plug it in and run: " +
				"virsh attach-device " + vmName + " <hostdev.xml> --persistent")
			continue
		}
		vp := strings.SplitN(id, ":", 2)
		xml := fmt.Sprintf(`<hostdev mode='subsystem' type='usb' managed='yes'>
  <source>
    <vendor id='0x%s'/>
    <product id='0x%s'/>
  </source>
</hostdev>
`, vp[0], vp[1])
		f, err := os.CreateTemp("", "vmx-usb-*.xml")
		if err != nil {
			log("usb: " + err.Error())
			continue
		}
		_, _ = f.WriteString(xml)
		_ = f.Close()
		// --live so the recipe running in cloud-init can already see the
		// radio; --persistent so a reboot keeps it.
		if out, err := sudoRun("virsh", "attach-device", vmName, f.Name(),
			"--live", "--persistent"); err != nil {
			log("usb: attach " + id + " failed — " + strings.SplitN(out, "\n", 2)[0])
		} else {
			log("usb: " + id + " attached (live + persistent)")
		}
		_ = os.Remove(f.Name())
	}
}
