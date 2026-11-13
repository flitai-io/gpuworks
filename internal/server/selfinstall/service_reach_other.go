//go:build !linux

package selfinstall

// Only systemd installs are checked, and systemd is Linux's.
func statDirAccess(string) (dirAccess, error) { return dirAccess{}, errUnsupportedStat }
