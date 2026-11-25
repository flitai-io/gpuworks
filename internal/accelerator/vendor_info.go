package accelerator

// VendorInfo is what a vendor's definition declares, for whoever presents
// it: the console's vendor dictionary and the declared-support table of the
// support matrix. It carries no rule; the rules are asked of this package's
// functions.
type VendorInfo struct {
	ID    string
	Brand string
	// Frameworks are the image frameworks that belong to the vendor.
	Frameworks []string
	// Tool is the vendor tool's overview command, the one an operator is
	// pointed to; "" when it has none. A driver may read its cards another
	// way.
	Tool string
	// HostOS and HostArch are the operating system and architecture its
	// cards carry workloads on, each "" when the vendor does not restrict it.
	HostOS, HostArch string
	// Runtimes are the runtime drivers its cards carry workloads on; empty
	// for a vendor whose cards are found and monitored only.
	Runtimes []string
	// Features are the platform features offered on its cards.
	Features []Feature
	// PartitionBackend is its dGPU backend's id, "" when cards are given
	// out whole, and PartitionRuntimes the runtimes that backend runs on.
	PartitionBackend  string
	PartitionRuntimes []string
}

// Vendors describes every vendor, in KnownVendors' order.
func Vendors() []VendorInfo {
	out := make([]VendorInfo, 0, len(vendorSpecs))
	for _, spec := range vendorSpecs {
		info := VendorInfo{
			ID: spec.id, Brand: spec.brand, Tool: spec.tool,
			HostOS: spec.hostOS, HostArch: spec.hostArch,
			Frameworks: append([]string(nil), spec.frameworks...),
			Runtimes:   append([]string(nil), spec.runtimes...),
			Features:   append([]Feature(nil), spec.supports...),
		}
		if spec.partition != nil {
			info.PartitionBackend = spec.partition.ID()
			info.PartitionRuntimes = info.Runtimes
			if limited, ok := spec.partition.(RuntimeConstrainer); ok {
				info.PartitionRuntimes = limited.SupportedRuntimes()
			}
		}
		out = append(out, info)
	}
	return out
}
