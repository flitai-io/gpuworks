// Package hami reads the HAMi-core (libvgpu.so) shared-usage cache that the
// NVIDIA partition shim maintains per instance, for per-GPU memory and SM
// utilization reporting. The injection itself lives in the agent engine's
// NVIDIA partition injector; this package owns only the on-disk cache format.
package dgpucache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// These offsets mirror HAMi-core shared_region_t v1.2 on 64-bit Linux. The
// packaged libvgpu must be kept on this layout; rejecting any other version is
// intentional because silently reading a shifted mmap would fabricate usage.
const (
	cacheMagic        = uint32(19920718)
	cacheMajorVersion = uint32(1)
	cacheMinorVersion = uint32(2)
	cacheMaxDevices   = 16
	cacheMaxProcesses = 1024

	cacheMajorOffset    = 4
	cacheMinorOffset    = 8
	cacheLimitOffset    = 1600 // uint64 limit[cacheMaxDevices], read by every allocation check
	cacheProcessBase    = 1856
	cacheProcessSize    = 1960
	cacheProcessCountAt = cacheProcessBase + cacheMaxProcesses*cacheProcessSize

	cachePIDOffset          = 0
	cacheMemoryBase         = 8
	cacheDeviceMemorySize   = 64
	cacheMemoryTotalOffset  = 32
	cacheDeviceUtilBase     = 1160
	cacheDeviceUtilSize     = 48
	cacheSMUtilOffset       = 16
	cacheSequenceLockOffset = 1936
)

var (
	ErrCacheUninitialized = errors.New("dGPU usage cache is not initialized")
	ErrCacheVersion       = errors.New("unsupported dGPU usage cache version")
	// ErrLimitBelowUsage: a device already holds more than the limit asked
	// for. Lowering the limit under it would fail the engine's next
	// allocation, which ends the engine; the limits are left as they were.
	ErrLimitBelowUsage = errors.New("dGPU memory limit is below what the instance holds")
)

// DeviceUsage is the per-visible-device snapshot maintained by libvgpu.
type DeviceUsage struct {
	MemoryUsedBytes uint64
	SMUtilPercent   uint64
}

// CacheSnapshot is one consistent best-effort read of a HAMi-core shared cache.
type CacheSnapshot struct {
	MajorVersion uint32
	MinorVersion uint32
	Devices      []DeviceUsage
}

// ReadCache mmaps a per-instance HAMi-core cache and aggregates its active process
// slots. deviceCount is the number of visible devices in the instance spec.
func ReadCache(path string, deviceCount int) (CacheSnapshot, error) {
	if deviceCount <= 0 || deviceCount > cacheMaxDevices {
		return CacheSnapshot{}, fmt.Errorf("invalid HAMi-core device count %d", deviceCount)
	}
	f, err := os.Open(path)
	if err != nil {
		return CacheSnapshot{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return CacheSnapshot{}, err
	}
	if info.Size() < cacheProcessCountAt+4 {
		return CacheSnapshot{}, fmt.Errorf("HAMi cache too small: %d bytes", info.Size())
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return CacheSnapshot{}, fmt.Errorf("mmap HAMi cache: %w", err)
	}
	// This function owns the mapping; parsing/writing determines the result.
	defer func() { _ = syscall.Munmap(data) }()
	return parseCache(data, deviceCount)
}

func parseCache(data []byte, deviceCount int) (CacheSnapshot, error) {
	if len(data) < cacheProcessCountAt+4 {
		return CacheSnapshot{}, fmt.Errorf("HAMi cache too small: %d bytes", len(data))
	}
	if binary.LittleEndian.Uint32(data[:4]) != cacheMagic {
		return CacheSnapshot{}, ErrCacheUninitialized
	}
	major := binary.LittleEndian.Uint32(data[cacheMajorOffset : cacheMajorOffset+4])
	minor := binary.LittleEndian.Uint32(data[cacheMinorOffset : cacheMinorOffset+4])
	if major != cacheMajorVersion || minor != cacheMinorVersion {
		return CacheSnapshot{}, fmt.Errorf("%w: got %d.%d, want %d.%d", ErrCacheVersion, major, minor, cacheMajorVersion, cacheMinorVersion)
	}
	procCount := int(int32(binary.LittleEndian.Uint32(data[cacheProcessCountAt : cacheProcessCountAt+4])))
	if procCount < 0 || procCount > cacheMaxProcesses {
		return CacheSnapshot{}, fmt.Errorf("invalid HAMi process count %d", procCount)
	}
	out := CacheSnapshot{MajorVersion: major, MinorVersion: minor, Devices: make([]DeviceUsage, deviceCount)}
	for process := 0; process < procCount; process++ {
		base := cacheProcessBase + process*cacheProcessSize
		if int32(binary.LittleEndian.Uint32(data[base+cachePIDOffset:base+cachePIDOffset+4])) <= 0 {
			continue
		}
		usage, ok := readStableProcess(data, base, deviceCount)
		if !ok {
			continue
		}
		for device := range usage {
			out.Devices[device].MemoryUsedBytes += usage[device].MemoryUsedBytes
			out.Devices[device].SMUtilPercent += usage[device].SMUtilPercent
			if out.Devices[device].SMUtilPercent > 100 {
				out.Devices[device].SMUtilPercent = 100
			}
		}
	}
	return out, nil
}

func readStableProcess(data []byte, base, deviceCount int) ([]DeviceUsage, bool) {
	for attempt := 0; attempt < 3; attempt++ {
		seq1 := binary.LittleEndian.Uint64(data[base+cacheSequenceLockOffset : base+cacheSequenceLockOffset+8])
		if seq1&1 != 0 {
			continue
		}
		usage := make([]DeviceUsage, deviceCount)
		for device := 0; device < deviceCount; device++ {
			memoryAt := base + cacheMemoryBase + device*cacheDeviceMemorySize + cacheMemoryTotalOffset
			smAt := base + cacheDeviceUtilBase + device*cacheDeviceUtilSize + cacheSMUtilOffset
			usage[device] = DeviceUsage{
				MemoryUsedBytes: binary.LittleEndian.Uint64(data[memoryAt : memoryAt+8]),
				SMUtilPercent:   binary.LittleEndian.Uint64(data[smAt : smAt+8]),
			}
		}
		seq2 := binary.LittleEndian.Uint64(data[base+cacheSequenceLockOffset : base+cacheSequenceLockOffset+8])
		if seq1 == seq2 && seq2&1 == 0 {
			return usage, true
		}
	}
	return nil, false
}

// SetMemoryLimit makes limitBytes the memory limit of each of an instance's
// visible devices, in place. HAMi-core reads the limit from the shared cache
// on every allocation check and every memory query, so a running instance is
// held to the new limit from its next allocation on; what an engine already
// sized at start is not resized by this. It reports whether any limit
// changed. A device holding more than limitBytes refuses the whole change
// (ErrLimitBelowUsage, with what it holds), as does a cache HAMi-core has
// not initialised yet: its first process would overwrite the limits from
// its environment.
func SetMemoryLimit(path string, deviceCount int, limitBytes int64) (bool, error) {
	if deviceCount <= 0 || deviceCount > cacheMaxDevices {
		return false, fmt.Errorf("invalid HAMi-core device count %d", deviceCount)
	}
	if limitBytes <= 0 {
		return false, fmt.Errorf("invalid dGPU memory limit %d", limitBytes)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < cacheProcessCountAt+4 {
		return false, fmt.Errorf("HAMi cache too small: %d bytes", info.Size())
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return false, fmt.Errorf("mmap HAMi cache: %w", err)
	}
	// This function owns the mapping; parsing/writing determines the result.
	defer func() { _ = syscall.Munmap(data) }()
	return setLimit(data, deviceCount, uint64(limitBytes))
}

func setLimit(data []byte, deviceCount int, limit uint64) (bool, error) {
	snapshot, err := parseCache(data, deviceCount)
	if err != nil {
		return false, err
	}
	for device, usage := range snapshot.Devices {
		if usage.MemoryUsedBytes > limit {
			return false, fmt.Errorf("%w: device %d holds %d bytes, limit %d", ErrLimitBelowUsage, device, usage.MemoryUsedBytes, limit)
		}
	}
	changed := false
	for device := 0; device < deviceCount; device++ {
		at := cacheLimitOffset + device*8
		if binary.LittleEndian.Uint64(data[at:at+8]) != limit {
			// One aligned 8-byte store: a reader sees the old limit or the
			// new one, never a mix.
			binary.LittleEndian.PutUint64(data[at:at+8], limit)
			changed = true
		}
	}
	return changed, nil
}
