//go:build linux && arm64

package system

// MachineInfo describes the production board. It is a separate file from the
// affinity helpers because affinity is architecture-independent, and keeping
// the build tag on a 3-line machine description means a new architecture only
// has to add this file, never a copy of the syscall plumbing.
func MachineInfo() string { return "raspberry-pi-5 (bcm2712, cortex-a76)" }
