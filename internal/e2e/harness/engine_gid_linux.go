//go:build e2e && linux

package harness

import (
	"os"
	"strconv"
	"syscall"
)

// DockerSocketGID returns the GID that owns the file at path (typically the
// Docker socket) as a decimal string, and true when it could be determined.
// The in-container group uses it to mirror compose.example.yaml's group_add
// layout, so a fixture container can reach the bind-mounted socket without
// running as root.
func DockerSocketGID(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(stat.Gid), 10), true
}
