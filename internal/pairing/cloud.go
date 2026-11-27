package pairing

import (
	"os"
	"path/filepath"
	"strings"
)

// DMIDir is where Linux shows the machine's firmware (DMI) strings.
const DMIDir = "/sys/class/dmi/id"

// dmiFiles are the DMI strings CloudVendor reads.
var dmiFiles = []string{"sys_vendor", "product_name", "product_version", "bios_vendor", "bios_version", "board_vendor", "chassis_vendor", "chassis_asset_tag"}

// cloudMarks are lower-case substrings of those strings that name a cloud provider's VM.
var cloudMarks = []struct{ mark, name string }{
	{"amazon ec2", "Amazon EC2"}, {"amazon", "Amazon EC2"},
	{"google compute engine", "Google Compute Engine"}, {"google", "Google Compute Engine"},
	{"7783-7084-3265-9085-8269-3286-77", "Microsoft Azure"}, // Azure's chassis asset tag
	{"digitalocean", "DigitalOcean"}, {"hetzner", "Hetzner"}, {"linode", "Linode"}, {"akamai", "Akamai"},
	{"vultr", "Vultr"}, {"oraclecloud", "Oracle Cloud"}, {"scaleway", "Scaleway"},
	{"alibaba cloud", "Alibaba Cloud"}, {"openstack", "OpenStack"}, {"upcloud", "UpCloud"},
	{"exoscale", "Exoscale"},
}

// CloudVendor names the cloud provider whose VM this is, from the DMI strings in dir (DMIDir on
// Linux), or "" when none matches or dir does not exist (macOS, containers without /sys). It
// is a hint, not proof: a host it misses still gets the cloud note (transport.md 6.1.3).
func CloudVendor(dir string) string {
	var b strings.Builder
	for _, f := range dmiFiles {
		v, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || len(v) > 256 {
			continue
		}
		b.WriteString(strings.ToLower(strings.TrimSpace(string(v))))
		b.WriteByte('\n')
	}
	all := b.String()
	for _, m := range cloudMarks {
		if strings.Contains(all, m.mark) {
			return m.name
		}
	}
	return ""
}
