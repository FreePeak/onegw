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
// The commands run on the HOST. The recreate must mount the SAME /data
// volume the current container uses: a recreate without it starts a fresh,
// empty usage.db and orphans the old data (the anonymous-volume fork that
// VOLUME declarations used to cause — see the Dockerfile note). compose
// users keep their named volume automatically; plain docker run users must
// carry the -v flags over.
func ContainerGuidance(rel *Release) string {
	return fmt.Sprintf(`onegw %s is running in a container: binary self-update does not
apply (the filesystem belongs to the image). Update on the host:

  docker pull %s:%s
  docker compose up -d        # if started via docker compose
  # otherwise recreate, mounting the SAME data volume:
  docker stop onegw && docker rm onegw
  docker run -d --name onegw --restart unless-stopped -p 8080:8080 \
    -v onegw-data:/data %s:%s

Usage history lives in the mounted /data volume: recreating without the
same mount starts a fresh empty usage.db and orphans the old one. If the
previous container ran WITHOUT -v, copy the data out first
(docker cp onegw:/data/usage.db .) before removing it. Auto-apply is
ignored in containers; this instance will keep checking and log newer
releases.`, rel.Tag, Image, rel.Tag, Image, rel.Tag)
}
