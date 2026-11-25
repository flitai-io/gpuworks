package dto

// AcceleratorVendor is one entry of the vendor dictionary the console reads
// instead of carrying its own list: who the vendor is and what its
// definition declares.
type AcceleratorVendor struct {
	ID    string `json:"id"`
	Brand string `json:"brand"`
	// Frameworks are the image frameworks that belong to the vendor.
	Frameworks []string `json:"frameworks"`
	// Runtimes are the runtime drivers its cards carry workloads on; empty
	// for a vendor whose cards are found and monitored only.
	Runtimes []string `json:"runtimes"`
	// Features are the platform features offered on its cards.
	Features []string `json:"features"`
	// PartitionBackend is its dGPU backend, omitted when cards are given out
	// whole.
	PartitionBackend string `json:"partition_backend,omitempty"`
}
