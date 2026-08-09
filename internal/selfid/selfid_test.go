package selfid

import (
	"os"
	"path/filepath"
	"testing"
)

const fakeContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// writeProcFile writes content to <root>/proc/self/<name>, creating parent
// directories as needed.
func writeProcFile(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "proc", "self")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
}

// markDockerenv creates <root>/.dockerenv so Detect reports Containerized.
func markDockerenv(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, ".dockerenv")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
}

func emptyGetenv(string) string { return "" }

func TestDetectNotContainerized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "no dockerenv marker in temp root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()

			// Act
			got := Detect(root, "", emptyGetenv)

			// Assert
			if got.Containerized {
				t.Errorf("Containerized = %v, want false", got.Containerized)
			}
		})
	}
}

func TestDetectContainerizedEmptyProc(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "dockerenv present but no /proc files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			markDockerenv(t, root)

			// Act
			got := Detect(root, "", emptyGetenv)

			// Assert
			if !got.Containerized {
				t.Errorf("Containerized = %v, want true", got.Containerized)
			}
			if len(got.Candidates) != 0 {
				t.Errorf("Candidates = %v, want empty", got.Candidates)
			}
		})
	}
}

func TestDetectFromMountinfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
	}{
		{
			name: "real-shaped mountinfo line",
			line: "640 35 0:56 / /var/lib/docker/containers/" + fakeContainerID +
				"/hostname rw,nosuid,nodev - ext4 /dev/sda1 rw\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			markDockerenv(t, root)
			writeProcFile(t, root, "mountinfo", tt.line)

			// Act
			got := Detect(root, "", emptyGetenv)

			// Assert
			if len(got.Candidates) == 0 || got.Candidates[0] != fakeContainerID {
				t.Errorf("Candidates = %v, want [%s, ...]", got.Candidates, fakeContainerID)
			}
		})
	}
}

func TestDetectFromCgroupV1(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
	}{
		{
			name: "cgroup v1 systemd controller line",
			line: "1:name=systemd:/docker/" + fakeContainerID + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			markDockerenv(t, root)
			writeProcFile(t, root, "cgroup", tt.line)

			// Act
			got := Detect(root, "", emptyGetenv)

			// Assert
			if len(got.Candidates) != 1 || got.Candidates[0] != fakeContainerID {
				t.Errorf("Candidates = %v, want [%s]", got.Candidates, fakeContainerID)
			}
		})
	}
}

func TestDetectCgroupV2Empty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
	}{
		{name: "cgroup v2 private namespace line", line: "0::/\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			markDockerenv(t, root)
			writeProcFile(t, root, "cgroup", tt.line)

			// Act
			got := Detect(root, "", emptyGetenv)

			// Assert
			if len(got.Candidates) != 0 {
				t.Errorf("Candidates = %v, want empty", got.Candidates)
			}
		})
	}
}

func TestDetectHostnameShortID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		hostname string
	}{
		{name: "12-hex hostname accepted", hostname: "abc123def456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			getenv := func(key string) string {
				if key == "HOSTNAME" {
					return tt.hostname
				}
				return ""
			}

			// Act
			got := Detect(root, "", getenv)

			// Assert
			if len(got.Candidates) != 1 || got.Candidates[0] != tt.hostname {
				t.Errorf("Candidates = %v, want [%s]", got.Candidates, tt.hostname)
			}
		})
	}
}

func TestDetectHostnameRejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		hostname string
	}{
		{name: "non-hex hostname rejected", hostname: "my-vps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			getenv := func(key string) string {
				if key == "HOSTNAME" {
					return tt.hostname
				}
				return ""
			}

			// Act
			got := Detect(root, "", getenv)

			// Assert
			if len(got.Candidates) != 0 {
				t.Errorf("Candidates = %v, want empty", got.Candidates)
			}
		})
	}
}

func TestDetectOverrideWins(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		override string
	}{
		{name: "override pre-empts every other source", override: "operator-supplied-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			markDockerenv(t, root)
			writeProcFile(t, root, "mountinfo", fakeContainerID+"\n")
			getenv := func(key string) string {
				if key == "HOSTNAME" {
					return "abc123def456"
				}
				return ""
			}

			// Act
			got := Detect(root, tt.override, getenv)

			// Assert
			if len(got.Candidates) == 0 || got.Candidates[0] != tt.override {
				t.Errorf("Candidates[0] = %v, want %s", got.Candidates, tt.override)
			}
		})
	}
}

func TestDetectDeduplicates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "same ID present in mountinfo and cgroup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			root := t.TempDir()
			markDockerenv(t, root)
			writeProcFile(t, root, "mountinfo", fakeContainerID+"\n")
			writeProcFile(t, root, "cgroup", "1:name=systemd:/docker/"+fakeContainerID+"\n")

			// Act
			got := Detect(root, "", emptyGetenv)

			// Assert
			if len(got.Candidates) != 1 {
				t.Errorf("len(Candidates) = %d, want 1: %v", len(got.Candidates), got.Candidates)
			}
		})
	}
}
