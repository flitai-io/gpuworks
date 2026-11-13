package backupexclude

import "golang.org/x/sys/unix"

// excludeAttr is the extended attribute `tmutil addexclusion` sets: an item
// carrying it is already excluded, which reading it tells without tmutil.
const excludeAttr = "com.apple.metadata:com_apple_backup_excludeItem"

func hasXattr(path string) bool {
	n, err := unix.Getxattr(path, excludeAttr, nil)
	return err == nil && n > 0
}
