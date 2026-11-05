//go:build linux && arm

package system

// sysGetCPU is the getcpu(2) syscall number for 32-bit ARM. The Raspberry Pi
// 3B (Cortex-A53, armv7l) runs this ABI, so the dashboard build needs it just
// as the Pi 5 needs the arm64 number. Kept next to the amd64/arm64 constants so
// all three are reviewed together.
const sysGetCPU = 345
