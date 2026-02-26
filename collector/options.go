package collector

// Options holds configuration options shared by all collectors,
// primarily for running inside containers where host paths are mounted
// at non-standard locations.
type Options struct {
	// ProcPath is the procfs mount point (default "/proc", use "/host/proc" in containers).
	ProcPath string

	// RootfsPath is the host root filesystem mount point (default "/", use "/host" in containers).
	// When set to something other than "/", commands are executed via chroot.
	RootfsPath string
}

// IsContainer returns true when the exporter seems to be running inside a container
// (i.e. rootfs is not "/").
func (o Options) IsContainer() bool {
	return o.RootfsPath != "" && o.RootfsPath != "/"
}
