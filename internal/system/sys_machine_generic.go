//go:build linux && !arm64

package system

// MachineInfo names the development host class. Only the board description is
// architecture-specific (see sys_machine_arm64.go); the syscall plumbing in
// sys_affinity_linux.go is shared, so a new architecture adds one file, not two.
func MachineInfo() string { return "linux/" + archName }
