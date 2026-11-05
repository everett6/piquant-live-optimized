//go:build linux

package system

// pinning works on every Linux arch: CPU affinity is a plain syscall.
const pinningSupported = true

// setAffinity restricts the calling thread to the CPUs in mask.
func setAffinity(mask *uint64) error { return setAffinityErr(mask) }
