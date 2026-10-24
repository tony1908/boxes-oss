package types

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Drive is a named persistent volume that boxes can mount. Its data survives box deletion.
type Drive struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// Mount attaches a drive to a box at Path.
type Mount struct {
	Drive    string `json:"drive"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// MaxMounts is the maximum number of drive mounts a box may carry.
const MaxMounts = 4

// mountRoot is the only directory drives may be mounted under. ponytail: restricted to /mnt so drive
// data never lands in /config snapshots or shadows system dirs.
const mountRoot = "/mnt/"

var drivePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateDriveName checks a drive name against the drive name pattern.
func ValidateDriveName(name string) error {
	if !drivePattern.MatchString(name) {
		return fmt.Errorf("drive name %q must match %s", name, drivePattern.String())
	}
	return nil
}

// ValidateMounts checks mount count, drive names, and mount paths. It does not check that drives exist.
func ValidateMounts(mounts []Mount) error {
	if len(mounts) > MaxMounts {
		return fmt.Errorf("mounts must not exceed %d entries", MaxMounts)
	}
	for i, mount := range mounts {
		if err := ValidateDriveName(mount.Drive); err != nil {
			return err
		}
		if !strings.HasPrefix(mount.Path, mountRoot) || path.Clean(mount.Path) != mount.Path || mount.Path == mountRoot {
			return fmt.Errorf("mount path %q must be a clean absolute path under /mnt/", mount.Path)
		}
		if strings.ContainsAny(mount.Path, ",=\x00") {
			return fmt.Errorf("mount path %q must not contain ',', '=' or NUL", mount.Path)
		}
		for _, other := range mounts[:i] {
			if strings.HasPrefix(mount.Path, other.Path+"/") || strings.HasPrefix(other.Path, mount.Path+"/") || mount.Path == other.Path {
				return fmt.Errorf("mount paths %q and %q must not be duplicated or nested", other.Path, mount.Path)
			}
		}
	}
	return nil
}
