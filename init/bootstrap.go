// Copyright 2015 Apcera Inc. All rights reserved.

package init

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/apcera/kurma/remote"
	"github.com/apcera/kurma/stage1/container"
	"github.com/apcera/kurma/stage1/server"
	"github.com/apcera/util/proc"
	"github.com/vishvananda/netlink"
)

// loadConfigurationFile loads a configuration file on disk and meges it with
// the default configuration. This often acts as the per runtime environment
// configuration.
func (r *runner) loadConfigurationFile() error {
	f, err := os.Open(configurationFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var diskConfig *kurmaConfig
	if err := json.NewDecoder(f).Decode(&diskConfig); err != nil {
		return err
	}

	r.config.mergeConfig(diskConfig)
	return nil
}

// createSystemMounts configured the default mounts for the host. Since kurma is
// running as PID 1, there is no /etc/fstab, therefore it must mount them
// itself.
func (r *runner) createSystemMounts() error {
	// Default mounts to handle on boot. Note that order matters, they should be
	// alphabetical by mount location. Elements are: mount location, source,
	// fstype.
	systemMounts := [][]string{
		[]string{"/dev", "devtmpfs", "devtmpfs"},
		[]string{"/dev/pts", "none", "devpts"},
		[]string{"/proc", "none", "proc"},
		[]string{"/sys", "none", "sysfs"},
		[]string{"/tmp", "none", "tmpfs"},
		[]string{"/var/kurma", "none", "tmpfs"},

		// put cgroups in a tmpfs so we can create the subdirectories
		[]string{cgroupsMount, "none", "tmpfs"},
	}

	r.log.Info("Creating system mounts")

	// Check if the /proc/mounts file exists to see if there are mounts that
	// already exist. This is primarily to support testing bootstrapping with
	// kurma launched by kurma (yes, meta)
	var existingMounts map[string]*proc.MountPoint
	if _, err := os.Lstat(proc.MountProcFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to check if %q existed: %v", proc.MountProcFile, err)
	} else if os.IsNotExist(err) {
		// really are freshly booted, /proc isn't mounted, so make this blank
		existingMounts = make(map[string]*proc.MountPoint)
	} else {
		// Get existing mount points.
		existingMounts, err = proc.MountPoints()
		if err != nil {
			return fmt.Errorf("failed to read existing mount points: %v", err)
		}
	}

	for _, mount := range systemMounts {
		location, source, fstype := mount[0], mount[1], mount[2]

		// check if it exists
		if _, exists := existingMounts[location]; exists {
			r.log.Tracef("- skipping %q, already mounted", location)
			continue
		}

		// perform the mount
		r.log.Tracef("- mounting %q (type %q) to %q", source, fstype, location)
		if err := handleMount(source, location, fstype, ""); err != nil {
			return fmt.Errorf("failed to mount %q: %v", location, err)
		}
	}
	return nil
}

// configureEnvironment sets environment variables that will be necessary for
// the process.
func (r *runner) configureEnvironment() error {
	os.Setenv("TMPDIR", "/tmp")
	return nil
}

// mountCgroups handles creating the individual cgroup endpoints that are
// necessary.
func (r *runner) mountCgroups() error {
	// Default cgroups to mount and utilize.
	cgroupTypes := []string{
		"blkio",
		"cpu",
		"cpuacct",
		"devices",
		"memory",
	}

	r.log.Info("Setting up cgroups")

	// mount the cgroups
	for _, cgrouptype := range cgroupTypes {
		location := filepath.Join(cgroupsMount, cgrouptype)
		r.log.Tracef("- mounting cgroup %q to %q", cgrouptype, location)
		if err := handleMount("none", location, "cgroup", cgrouptype); err != nil {
			return fmt.Errorf("failed to mount cgroup %q: %v", cgrouptype, err)
		}

		// if this is the memory mount, need to set memory.use_hierarchy = 1
		if cgrouptype == "memory" {
			err := func() error {
				hpath := filepath.Join(location, "memory.use_hierarchy")
				f, err := os.OpenFile(hpath, os.O_WRONLY|os.O_TRUNC, os.FileMode(0644))
				if err != nil {
					return fmt.Errorf("Failed to configure memory hierarchy: %v", err)
				}
				defer f.Close()
				if _, err := f.WriteString("1\n"); err != nil {
					return fmt.Errorf("Failed to configure memory heirarchy: %v", err)
				}
				return nil
			}()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// loadModules handles loading all of the kernel modules that are specified in
// the configuration.
func (r *runner) loadModules() error {
	if len(r.config.Modules) == 0 {
		return nil
	}

	r.log.Infof("Loading specified modules [%s]", strings.Join(r.config.Modules, ", "))
	for _, mod := range r.config.Modules {
		if err := exec.Command("/sbin/modprobe", mod).Run(); err != nil {
			r.log.Errorf("- Failed to load module: %s", mod)
		}
	}
	return nil
}

// configureHostname calls to set the hostname to the one provided via
// configuration.
func (r *runner) configureHostname() error {
	if r.config.Hostname == "" {
		return nil
	}

	r.log.Infof("Setting hostname: %s", r.config.Hostname)
	if err := syscall.Sethostname([]byte(r.config.Hostname)); err != nil {
		r.log.Errorf("- Failed to set hostname: %v", err)
	}
	return nil
}

// configureNetwork handles iterating the local interfaces, matching it to an
// interface configuration, and configuring it. It will also handle configuring
// the default gateway after all interfaces are configured.
func (r *runner) configureNetwork() error {
	if r.config.NetworkConfig == nil {
		r.log.Warn("No network configuration given, skipping")
		return nil
	}

	links, err := netlink.LinkList()
	if err != nil {
		return err
	}

	for _, link := range links {
		linkName := link.Attrs().Name
		r.log.Infof("Configuring %s...", linkName)

		// look for a matching network config entry
		var netconf *kurmaNetworkInterface
		for _, n := range r.config.NetworkConfig.Interfaces {
			if linkName == n.Device {
				netconf = n
				break
			}
			if match, _ := regexp.MatchString(n.Device, linkName); match {
				netconf = n
				break
			}
		}

		// handle if none are found
		if netconf == nil {
			r.log.Warn("- no matching network configuraton found")
			continue
		}

		// configure it
		if err := configureInterface(link, netconf); err != nil {
			r.log.Warnf("- %s", err.Error())
		}
	}

	// configure the gateway
	if r.config.NetworkConfig.Gateway != "" {
		gateway := net.ParseIP(r.config.NetworkConfig.Gateway)
		if gateway == nil {
			r.log.Warnf("Failed to configure gatway to %q", r.config.NetworkConfig.Gateway)
		}

		route := &netlink.Route{
			Scope: netlink.SCOPE_UNIVERSE,
			Gw:    gateway,
		}
		if err := netlink.RouteAdd(route); err != nil {
			r.log.Warnf("Failed to configure gateway: %v", err)
			return nil
		}
		r.log.Infof("Configured gatway to %s", r.config.NetworkConfig.Gateway)
	}

	// configure DNS
	if len(r.config.NetworkConfig.DNS) > 0 {
		// write the resolv.conf
		if err := os.RemoveAll("/etc/resolv.conf"); err != nil {
			return err
		}
		f, err := os.OpenFile("/etc/resolv.conf", os.O_CREATE, os.FileMode(0644))
		if err != nil {
			return err
		}
		defer f.Close()
		for _, ns := range r.config.NetworkConfig.DNS {
			if _, err := fmt.Fprintf(f, "nameserver %s\n", ns); err != nil {
				return err
			}
		}
	}

	return nil
}

// createDirectories ensures the specified storage paths for containers or
// volumes exist.
func (r *runner) createDirectories() error {
	if r.config.Paths == nil {
		return nil
	}

	if r.config.Paths.Containers != "" {
		if err := os.MkdirAll(r.config.Paths.Containers, os.FileMode(0755)); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) readonly() error {
	return syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
}

func (r *runner) displayNetwork() error {
	fmt.Printf("INTERFACES:\n")

	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, in := range interfaces {
		fmt.Printf("\t%#v\n", in)
		ad, err := in.Addrs()
		if err != nil {
			return err
		}
		for _, a := range ad {
			fmt.Printf("\t\taddr: %v\n", a)
		}
	}
	return nil
}

// launchManager creates the container manager to allow containers to be
// launched.
func (r *runner) launchManager() error {
	mopts := &container.Options{
		ParentCgroupName:   r.config.ParentCgroupName,
		ContainerDirectory: r.config.Paths.Containers,
	}
	m, err := container.NewManager(mopts)
	if err != nil {
		return err
	}
	m.Log = r.log.Clone()
	r.manager = m
	r.log.Trace("Container Manager has been initialized.")

	os.Chdir("/var/kurma")
	return nil
}

// startInitContainers launches the initial containers that are specified in the
// configuration.
func (r *runner) startInitContainers() error {
	for _, img := range r.config.InitContainers {
		func() {
			f, err := remote.RetrieveImage(img)
			if err != nil {
				r.log.Errorf("Failed to retrieve image %q: %v", img, err)
				return
			}
			defer f.Close()

			manifest, err := findManifest(f)
			if err != nil {
				r.log.Errorf("Failed to find manifest in image %q: %v", img, err)
				return
			}

			if _, err := f.Seek(0, 0); err != nil {
				r.log.Errorf("Failed to set up %q: %v", img, err)
				return
			}

			if _, err := r.manager.Create("", manifest, f); err != nil {
				r.log.Errorf("Failed to start up %q: %v", img, err)
				return
			}
			r.log.Infof("Launched container %s", manifest.Name.String())
		}()
	}
	return nil
}

// startServer begins the main Kurma RPC server and will take over execution.
func (r *runner) startServer() error {
	opts := &server.Options{
		ContainerManager: r.manager,
	}

	s := server.New(opts)
	go s.Start()
	return nil
}
