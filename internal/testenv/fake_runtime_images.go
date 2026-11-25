package testenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// The fake runtime's image cache: what ImageExists reports, and what
// PullImage, LoadImage and SaveImage change or produce.

func (d *FakeDriver) SetArchiveSize(n int64) {
	d.mu.Lock()
	if n > 0 {
		d.archiveSize = n
	}
	d.mu.Unlock()
}

func (d *FakeDriver) ImageExists(_ context.Context, image string) (bool, error) {
	d.mu.Lock()
	hook, after := d.OnImageExistsHook, d.AfterImageExistsHook
	d.mu.Unlock()
	if hook != nil {
		present, err, handled := hook(image)
		if handled {
			if after != nil {
				after(image, present)
			}
			return present, err
		}
	}
	if err := d.fail("ImageExists"); err != nil {
		return false, err
	}
	d.mu.Lock()
	absent := d.imageAbsent
	loaded := d.loadedImages[image] || d.loadedAny
	d.mu.Unlock()
	// A fresh fake runtime starts without images. PullImage/LoadImage marks the
	// cache populated; SetImageAbsent is an explicit negative verification hook.
	present := !absent && loaded
	if after != nil {
		after(image, present)
	}
	return present, nil
}

func (d *FakeDriver) PullImage(_ context.Context, image string, onLine func(string)) error {
	d.mu.Lock()
	hook, after := d.OnPullImageHook, d.AfterPullImageHook
	d.mu.Unlock()
	if hook != nil {
		if err, handled := hook(image, onLine); handled {
			if err == nil {
				d.mu.Lock()
				if d.loadedImages == nil {
					d.loadedImages = map[string]bool{}
				}
				d.loadedImages[image] = true
				d.mu.Unlock()
			}
			if after != nil {
				after(image)
			}
			return err
		}
	}
	err := d.fail("PullImage")
	if err == nil {
		d.mu.Lock()
		if d.loadedImages == nil {
			d.loadedImages = map[string]bool{}
		}
		d.loadedImages[image] = true
		d.mu.Unlock()
	}
	if after != nil {
		after(image)
	}
	return err
}

// SaveImage and LoadImage use a bounded deterministic archive.  The bytes are
// intentionally not a Docker tar: the production transport only requires a
// streaming reader/writer, while tests should stay fast and hermetic.
func (d *FakeDriver) SaveImage(_ context.Context, _ string, w io.Writer) error {
	d.mu.Lock()
	size := d.archiveSize
	d.mu.Unlock()
	if size <= 0 {
		size = 8 << 20
	}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for written := int64(0); written < size; {
		want := int64(len(chunk))
		if remain := size - written; remain < want {
			want = remain
		}
		n, err := w.Write(chunk[:want])
		written += int64(n)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (d *FakeDriver) LoadImage(_ context.Context, _ string, r io.Reader) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	d.mu.Lock()
	d.loadedAny = true
	if d.imageFixedByLoad {
		d.imageAbsent, d.imageFixedByLoad = false, false
	}
	d.mu.Unlock()
	return nil
}

// TagImage names a pulled image under another reference, as `docker tag`
// does: the source must be in the cache, and the target then is too. A pull
// from a mirror or a copy of the image is tagged back to the reference the
// instance runs.
func (d *FakeDriver) TagImage(_ context.Context, source, target string) error {
	if err := d.fail("TagImage"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.imageAbsent || !(d.loadedImages[source] || d.loadedAny) {
		return fmt.Errorf("Error response from daemon: No such image: %s", source)
	}
	if d.loadedImages == nil {
		d.loadedImages = map[string]bool{}
	}
	d.loadedImages[target] = true
	return nil
}

// FixImageAtNextLoad ends SetImageAbsent with the next archive load: until
// then the runtime still says the image is missing, and the load after it
// really installs the image. SetImageAbsent(false) makes the image visible at
// once, so a node that already loaded an archive reports it present on its
// next presence report, before any retry has asked it to load again.
func (d *FakeDriver) FixImageAtNextLoad() { d.mu.Lock(); d.imageFixedByLoad = true; d.mu.Unlock() }

// ImageStoreRoot: the fake keeps images in memory, whatever runtime it is
// named after.
func (d *FakeDriver) ImageStoreRoot() string { return "" }
