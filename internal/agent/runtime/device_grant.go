package runtime

import (
	"path/filepath"
	"strconv"
	"strings"
)

// DeviceAccessAll is read, write and mknod: what a compute device needs.
const DeviceAccessAll = "rwm"

// Device is a host device node the workload gets at the same path.
type Device struct {
	Path string
	// Access is the device-cgroup access asked for (DeviceAccessAll). Empty
	// passes the node bare and leaves access to the runtime's default.
	Access string
}

// DockerValue is the node as the value of docker's --device option. The kube
// runtime's device plugin takes the same spelling.
func (d Device) DockerValue() string {
	if d.Access == "" {
		return d.Path
	}
	return d.Path + ":" + d.Path + ":" + d.Access
}

// DeviceGrant is the accelerator devices a workload is given, stated as
// facts about the host: whose cards, which device nodes, which runtime
// class, which supplementary groups. A device backend fills it in and each
// driver gives it to the container in its own way; it never holds the
// options of one container CLI.
type DeviceGrant struct {
	// Vendor is whose cards these are, by the accelerator package's
	// canonical name (gpu.ResolveInjection writes it). A driver that
	// prepares a container differently per vendor reads it rather than
	// inferring the vendor from Env and DockerArgs. It is empty when no
	// cards were granted and in a workload that was not built from a
	// resolved injection.
	Vendor string
	// RuntimeClass is the container runtime that injects the devices itself
	// (Ascend Docker Runtime), or the plain one a backend insists on.
	RuntimeClass string
	// ToolkitDevices are the host indexes asked of the NVIDIA Container
	// Toolkit, which renumbers them from zero inside the container.
	ToolkitDevices []int
	// Devices are the device nodes handed over, in the backend's order.
	Devices []Device
	// Groups are host group ids the workload's process joins to open them.
	Groups []int
	// Replaces says which accelerator settings among the workload's own
	// options the grant takes the place of.
	Replaces Replaced
}

// Replaced names accelerator settings a workload carries among its own
// options (an image's, a template's, a user's) that a grant supersedes. They
// are left out when the workload's options are read (OwnDockerArgs); the
// options themselves are not edited.
type Replaced struct {
	// ToolkitRequest and RuntimeClass: the workload's own toolkit request
	// and runtime class go. A grant that names toolkit devices or a runtime
	// class replaces the workload's own without saying so here; a backend
	// sets these to clear another vendor's when it names none itself.
	ToolkitRequest, RuntimeClass bool
	// Devices are host device paths the backend manages. The workload's own
	// mapping of one of them goes; any other device stays.
	Devices []string
}

// Empty reports whether the grant hands nothing over: no runtime class, no
// toolkit devices, no device nodes and no groups.
func (g DeviceGrant) Empty() bool {
	return g.RuntimeClass == "" && len(g.ToolkitDevices) == 0 && len(g.Devices) == 0 && len(g.Groups) == 0
}

// Clone returns a grant that shares no slice with g.
func (g DeviceGrant) Clone() DeviceGrant {
	g.ToolkitDevices = append([]int(nil), g.ToolkitDevices...)
	g.Devices = append([]Device(nil), g.Devices...)
	g.Groups = append([]int(nil), g.Groups...)
	g.Replaces.Devices = append([]string(nil), g.Replaces.Devices...)
	return g
}

// ToolkitDeviceList is ToolkitDevices as the toolkit takes them: comma
// separated.
func (g DeviceGrant) ToolkitDeviceList() string {
	values := make([]string, len(g.ToolkitDevices))
	for i, index := range g.ToolkitDevices {
		values[i] = strconv.Itoa(index)
	}
	return strings.Join(values, ",")
}

// DockerDeviceArgs renders the grant as `docker run` options, for the
// drivers whose CLI takes them (docker, nerdctl). The order is fixed:
// runtime class, toolkit devices, device nodes in the order they were
// granted, then groups.
func (g DeviceGrant) DockerDeviceArgs() []string {
	var args []string
	if g.RuntimeClass != "" {
		args = append(args, "--runtime", g.RuntimeClass)
	}
	if len(g.ToolkitDevices) > 0 {
		// Docker parses --gpus values as CSV. Without the inner quotes,
		// device=0,1 becomes DeviceIDs=[0] plus Count=1 and recent daemons
		// reject the request as "cannot set both Count and DeviceIDs". exec
		// does not add shell quoting for us, so the quote characters are
		// intentional.
		args = append(args, "--gpus", `"device=`+g.ToolkitDeviceList()+`"`)
	}
	for _, device := range g.Devices {
		args = append(args, "--device", device.DockerValue())
	}
	for _, group := range g.Groups {
		args = append(args, "--group-add", strconv.Itoa(group))
	}
	return args
}

// DockerRunArgs is everything the workload asks of `docker run` beyond its
// image and command: its own options, then the devices it was granted.
func (s WorkloadSpec) DockerRunArgs() []string {
	return append(s.OwnDockerArgs(), s.Grant.DockerDeviceArgs()...)
}

// OwnDockerArgs is the workload's own options without the accelerator
// settings its grant replaces. The granted selection is authoritative: an
// image-level `--gpus all` does not remain beside the cards the scheduler
// chose, nor a runtime beside the one the backend names, nor a mapping of a
// device the backend manages.
func (s WorkloadSpec) OwnDockerArgs() []string {
	args := append([]string(nil), s.DockerArgs...)
	if len(s.Grant.ToolkitDevices) > 0 || s.Grant.Replaces.ToolkitRequest {
		args = withoutDockerOption(args, "--gpus")
	}
	if s.Grant.RuntimeClass != "" || s.Grant.Replaces.RuntimeClass {
		args = withoutDockerOption(args, "--runtime")
	}
	if len(s.Grant.Replaces.Devices) > 0 {
		args = withoutDockerDevices(args, s.Grant.Replaces.Devices)
	}
	return args
}

// withoutDockerOption returns args without an option that takes a value, in
// either spelling (`--opt value`, `--opt=value`).
func withoutDockerOption(args []string, option string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, option+"=") {
			continue
		}
		if arg == option {
			if index+1 < len(args) {
				index++
			}
			continue
		}
		result = append(result, arg)
	}
	return result
}

// withoutDockerDevices removes only --device mappings whose host source
// names one of paths. Other devices and malformed options are preserved, so
// unrelated access such as /dev/infiniband stays.
func withoutDockerDevices(args, paths []string) []string {
	managed := make(map[string]bool, len(paths))
	for _, path := range paths {
		managed[filepath.Clean(path)] = true
	}
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--device" {
			if index+1 == len(args) {
				result = append(result, arg)
				continue
			}
			value := args[index+1]
			if managedDockerDevice(value, managed) {
				index++
				continue
			}
			result = append(result, arg, value)
			index++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--device="); ok && managedDockerDevice(value, managed) {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func managedDockerDevice(value string, managed map[string]bool) bool {
	// DRM by-path aliases contain the colon in their PCI BDF, so split-at-first
	// colon parsing does not recover the source path. Match declared full source
	// paths first, then the source before the first colon.
	for path := range managed {
		if value == path || strings.HasPrefix(value, path+":") {
			return true
		}
	}
	return managed[filepath.Clean(strings.SplitN(value, ":", 2)[0])]
}

// OwnOptions is what a workload's own docker options ask for, of the kinds a
// device backend has to check before it grants (a compute network's RDMA
// device, the capability and the limit that go with it).
type OwnOptions struct {
	// Devices are --device values as written.
	Devices []string
	// Capabilities are --cap-add values.
	Capabilities []string
	// Ulimits are --ulimit values, "name=soft[:hard]".
	Ulimits []string
}

// ParseOwnOptions reads those options out of args, in either spelling
// (`--opt value`, `--opt=value`).
func ParseOwnOptions(args []string) OwnOptions {
	var own OwnOptions
	for i := 0; i < len(args); i++ {
		key, value, joined := strings.Cut(args[i], "=")
		var into *[]string
		switch key {
		case "--device":
			into = &own.Devices
		case "--cap-add":
			into = &own.Capabilities
		case "--ulimit":
			into = &own.Ulimits
		default:
			continue
		}
		if !joined {
			if i+1 >= len(args) {
				continue
			}
			i++
			value = args[i]
		}
		*into = append(*into, value)
	}
	return own
}
