package accelerator

import "testing"

func TestMetaXIdentityFromModelAndAliases(t *testing.T) {
	for _, name := range []string{"MXC550", "MetaX MXC550", "  mxc400  "} {
		if got := InferVendor("", name, "", ""); got != VendorMetaX {
			t.Errorf("InferVendor(%q) = %q", name, got)
		}
	}
	for _, alias := range []string{"MACA", "MACA 3.8", "MetaX", "MXC550"} {
		if got := NormalizeVendor(alias); got != VendorMetaX {
			t.Errorf("NormalizeVendor(%q) = %q", alias, got)
		}
	}
	for _, alias := range []string{"macaron", "unrelated-maca-label", "metaxylophone"} {
		if got := NormalizeVendor(alias); got == VendorMetaX {
			t.Errorf("unrelated %q classified MetaX", alias)
		}
	}
	if got := InferVendor("AMD", "MXC550", "", ""); got != VendorAMD {
		t.Errorf("explicit vendor lost: %s", got)
	}
}
