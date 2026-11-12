package docker

import (
	"context"
	"strings"
)

// ImagePresence uses the same inspection as metadata collection, avoiding a
// second CLI process per catalog image on every inventory snapshot.
func (d *Driver) ImagePresence(ctx context.Context, ref string) (bool, string, *int64, error) {
	image, err := d.InspectImage(ctx, ref)
	if err != nil {
		if ctx.Err() == nil && strings.Contains(strings.ToLower(err.Error()), "no such image:") {
			return false, "", nil, nil
		}
		return false, "", nil, err
	}
	return true, image.ID, image.RuntimeSizeBytes, nil
}
