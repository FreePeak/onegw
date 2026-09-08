package update

import (
	"fmt"
	"os"
)

// InContainer reports whether this process runs inside a container
// (Docker, Podman, or an explicit ONEGW_IN_CONTAINER=1). Container
// filesystems belong to the image: the binary at /usr/local/bin/onegw is
// root-owned and any in-place replacement is lost on the next recreate, so
// self-update inside a container degrades to check + host-side guidance.
func InContainer() bool {
	if os.Getenv("ONEGW_IN_CONTAINER") == "1" {
		return true
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Image is the published image reference the guidance prints.
const Image = "ghcr.io/freepeak/onegw"

// ContainerGuidance explains how to move a containerized onegw to rel.
// The commands run on the HOST; a named volume keeps usage data across the
// recreate, and `compose up -d` only replaces the container whose image
// changed.
func ContainerGuidance(rel *Release) string {
	return fmt.Sprintf(`onegw %s is running in a container: binary self-update does not
apply (the filesystem belongs to the image). Update on the host:

  docker pull %s:%s
  docker compose up -d        # if started via docker compose
  # otherwise: docker stop <name> && docker rm <name> && docker run ... %s:%s

Usage data survives on the %q volume. Auto-apply is ignored in
containers; this instance will keep checking and log newer releases.`,
		rel.Tag, Image, rel.Tag, Image, rel.Tag, "/data")
}
