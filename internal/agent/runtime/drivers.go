package runtime

import "gpuworks.ai/gpuworks/internal/proto"

const (
	DriverDocker  = proto.RuntimeDriverDocker
	DriverNerdctl = proto.RuntimeDriverNerdctl
	DriverUdocker = proto.RuntimeDriverUdocker
	DriverKube    = proto.RuntimeDriverKube
	DriverHost    = proto.RuntimeDriverHost
)

// PlainRuntimeClass is the OCI runtime that injects nothing. A backend names
// it as its grant's runtime class so that a node whose default runtime
// injects another vendor's devices cannot add them. A pod has no such
// option: a runtime class there names an object the cluster defines, and the
// kubelet's default runtime is already the plain one, so the kube driver
// leaves it out.
const PlainRuntimeClass = "runc"

// DeviceView is which accelerator devices a container of a runtime sees,
// and so how the vendor's library inside it counts them. A device backend
// that grants cards differently per runtime reads it from the Target it is
// given instead of comparing the driver's name.
type DeviceView int

const (
	// ViewUnknown is what a name that is not one of Drivers() gets. No
	// registered driver has it.
	ViewUnknown DeviceView = iota
	// ViewNone: there is no container. Engines of the host runtime are host
	// processes, and no backend selects devices for them.
	ViewNone
	// ViewGranted: the container sees only the device nodes it was granted,
	// so the vendor's library counts them from zero.
	ViewGranted
	// ViewHost: the container sees the host's whole /dev and /sys (PRoot),
	// so the vendor's library counts every card on the host and a selection
	// has to name the host's own indexes or identities.
	ViewHost
)

// DriverInfo is one runtime driver's entry.
type DriverInfo struct {
	Name string
	View DeviceView
	// HoldsImages: it keeps container images of its own, which a node that
	// switched to another runtime can still copy from.
	HoldsImages bool
	// Note is appended to the driver's name in the log line that says
	// automatic detection chose it.
	Note string
	// unavailable says why the driver cannot run on an OS, or "".
	unavailable func(goos string) string
}

var drivers = []DriverInfo{
	{
		// Engines run as host processes from macOS environment packages.
		Name: DriverHost, View: ViewNone, Note: "macOS host processes",
		unavailable: func(goos string) string {
			if goos != "darwin" {
				return "the host runtime runs macOS environment packages and is only available on macOS"
			}
			return ""
		},
	},
	{Name: DriverDocker, View: ViewGranted, HoldsImages: true},
	{Name: DriverNerdctl, View: ViewGranted, HoldsImages: true, Note: "no docker daemon"},
	{
		// runveil is PRoot on Linux ptrace, which macOS lacks.
		Name: DriverUdocker, View: ViewHost, HoldsImages: true, Note: "no docker daemon",
		unavailable: func(goos string) string {
			if goos == "darwin" {
				return "the runveil runtime needs Linux; a Mac uses the host runtime"
			}
			return ""
		},
	},
	// Never detected automatically: an agent is a Kubernetes node's because
	// it was deployed as one.
	{Name: DriverKube, View: ViewGranted},
}

// Drivers lists every runtime driver. Automatic detection tries them in
// this order.
func Drivers() []DriverInfo { return append([]DriverInfo(nil), drivers...) }

// DriverNamed finds a driver by the exact name its Name method returns.
func DriverNamed(name string) (DriverInfo, bool) {
	for _, d := range drivers {
		if d.Name == name {
			return d, true
		}
	}
	return DriverInfo{}, false
}

// Target is the runtime a device grant is for: the driver's name, for the
// tables keyed by it, and which devices its containers see.
type Target struct {
	Name string
	View DeviceView
}

// TargetNamed is the target of the driver registered under name. Its view is
// ViewUnknown for any other name, and no devices are granted for it.
func TargetNamed(name string) Target {
	d, _ := DriverNamed(name)
	return Target{Name: name, View: d.View}
}

// DeviceViewer is a driver that is not one of Drivers() (a stand-in for a
// runtime in a test or a simulation) saying which devices its containers
// see. A registered driver's view is its entry's.
type DeviceViewer interface {
	DeviceView() DeviceView
}

// ImageStoreRooter is a stand-in driver saying where it keeps unpacked
// image layers, "" for nowhere of its own. Without it a stand-in would be
// taken to own the store of the runtime whose name it reports.
type ImageStoreRooter interface {
	ImageStoreRoot() string
}

// TargetOf is the target a driver is; the zero Target for no driver.
func TargetOf(d Driver) Target {
	if d == nil {
		return Target{}
	}
	target := TargetNamed(d.Name())
	if viewer, ok := d.(DeviceViewer); ok && target.View == ViewUnknown {
		target.View = viewer.DeviceView()
	}
	return target
}

// UnavailableOn says why the driver cannot run on goos, or "".
func (d DriverInfo) UnavailableOn(goos string) string {
	if d.unavailable == nil {
		return ""
	}
	return d.unavailable(goos)
}
