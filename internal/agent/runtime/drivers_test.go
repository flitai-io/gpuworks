package runtime

import (
	"reflect"
	"testing"
)

// Every registered driver says which devices its containers see. A driver
// added without a view would be given whatever each device backend does for
// a runtime it knows nothing about.
func TestEveryRegisteredDriverHasADeviceView(t *testing.T) {
	names := map[string]bool{}
	for _, d := range Drivers() {
		if names[d.Name] {
			t.Errorf("driver %q is listed twice", d.Name)
		}
		names[d.Name] = true
		if d.View == ViewUnknown || TargetNamed(d.Name) != (Target{Name: d.Name, View: d.View}) {
			t.Errorf("driver %q has device view %v (target: %+v); state one", d.Name, d.View, TargetNamed(d.Name))
		}
	}
	for _, name := range []string{"", "Docker", "podman", " docker"} {
		if got := TargetNamed(name).View; got != ViewUnknown {
			t.Errorf("TargetNamed(%q).View = %v, want unknown", name, got)
		}
	}
}

// Automatic detection tries the host runtime first and runveil last; a
// driver is refused on an OS it cannot run on.
func TestDriverOrderAndAvailability(t *testing.T) {
	var order []string
	for _, d := range Drivers() {
		order = append(order, d.Name)
	}
	if want := []string{DriverHost, DriverDocker, DriverNerdctl, DriverUdocker, DriverKube}; !reflect.DeepEqual(order, want) {
		t.Fatalf("drivers are listed %v, want %v", order, want)
	}
	for _, c := range []struct {
		goos, driver string
		refused      bool
	}{
		{"linux", DriverHost, true}, {"darwin", DriverHost, false},
		{"darwin", DriverUdocker, true}, {"linux", DriverUdocker, false},
		{"linux", DriverDocker, false}, {"darwin", DriverDocker, false},
		{"linux", DriverNerdctl, false}, {"linux", DriverKube, false},
	} {
		d, ok := DriverNamed(c.driver)
		if !ok {
			t.Fatalf("no driver %q", c.driver)
		}
		if got := d.UnavailableOn(c.goos) != ""; got != c.refused {
			t.Errorf("%s on %s refused = %v, want %v", c.driver, c.goos, got, c.refused)
		}
	}
}

type standInDriver struct {
	Driver
	name string
	view DeviceView
}

func (d standInDriver) Name() string           { return d.name }
func (d standInDriver) DeviceView() DeviceView { return d.view }

type namedOnlyDriver struct {
	Driver
	name string
}

func (d namedOnlyDriver) Name() string { return d.name }

// A registered driver's view is its entry's, whatever a stand-in under its
// name says; a stand-in under another name states its own; a driver that
// states none has none, and neither has no driver.
func TestTargetOf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver Driver
		want   Target
	}{
		{"no driver", nil, Target{}},
		{"registered", namedOnlyDriver{name: DriverUdocker}, Target{Name: DriverUdocker, View: ViewHost}},
		{"registered name wins", standInDriver{name: DriverDocker, view: ViewHost}, Target{Name: DriverDocker, View: ViewGranted}},
		{"stand-in", standInDriver{name: "fake", view: ViewGranted}, Target{Name: "fake", View: ViewGranted}},
		{"unregistered without a view", namedOnlyDriver{name: "fake"}, Target{Name: "fake"}},
	} {
		if got := TargetOf(tc.driver); got != tc.want {
			t.Errorf("%s: TargetOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
