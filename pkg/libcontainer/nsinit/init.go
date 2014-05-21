// +build linux

package nsinit

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/dotcloud/docker/pkg/apparmor"
	"github.com/dotcloud/docker/pkg/label"
	"github.com/dotcloud/docker/pkg/libcontainer"
	"github.com/dotcloud/docker/pkg/libcontainer/console"
	"github.com/dotcloud/docker/pkg/libcontainer/mount"
	"github.com/dotcloud/docker/pkg/libcontainer/network"
	//"github.com/dotcloud/docker/pkg/libcontainer/security/capabilities"
	"github.com/dotcloud/docker/pkg/libcontainer/security/restrict"
	"github.com/dotcloud/docker/pkg/libcontainer/utils"
	"github.com/dotcloud/docker/pkg/system"
	"github.com/dotcloud/docker/pkg/user"
)

// Init is the init process that first runs inside a new namespace to setup mounts, users, networking,
// and other options required for the new container.
func Init(container *libcontainer.Container, uncleanRootfs, consolePath string, syncPipe *SyncPipe, args []string) error {
	rootfs, err := utils.ResolveRootfs(uncleanRootfs)
	if err != nil {
		return err
	}

	// clear the current processes env and replace it with the environment
	// defined on the container
	if err := LoadContainerEnvironment(container); err != nil {
		return err
	}

	// We always read this as it is a way to sync with the parent as well
	context, err := syncPipe.ReadFromParent()
	if err != nil {
		syncPipe.Close()
		return err
	}
	syncPipe.Close()

	if consolePath != "" {
		if err := console.OpenAndDup(consolePath); err != nil {
			return err
		}
	}
	if _, err := system.Setsid(); err != nil {
		return fmt.Errorf("setsid %s", err)
	}
	if consolePath != "" {
		if err := system.Setctty(); err != nil {
			return fmt.Errorf("setctty %s", err)
		}
	}
	if err := setupNetwork(container, context); err != nil {
		return fmt.Errorf("setup networking %s", err)
	}

	label.Init()

	if err := mount.InitializeMountNamespace(rootfs, consolePath, container); err != nil {
		return fmt.Errorf("setup mount namespace %s", err)
	}
	if container.Hostname != "" {
		if err := system.Sethostname(container.Hostname); err != nil {
			return fmt.Errorf("sethostname %s", err)
		}
	}

	logFile, err := os.OpenFile("/tmp/nsinit.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil
	}
	log := log.New(logFile, "NSINIT: ", log.Ldate|log.Ltime)
	runtime.LockOSThread()

	if err := apparmor.ApplyProfile(container.Context["apparmor_profile"]); err != nil {
		return fmt.Errorf("set apparmor profile %s: %s", container.Context["apparmor_profile"], err)
	}
	if err := label.SetProcessLabel(container.Context["process_label"]); err != nil {
		return fmt.Errorf("set process label %s", err)
	}
	if container.Context["restrictions"] != "" {
		if err := restrict.Restrict("sys"); err != nil {
			return err
		}
	}

	pdeathSignal, err := system.GetParentDeathSignal()
	if err != nil {
		return fmt.Errorf("get parent death signal %s", err)
	}

	/*
		if err := FinalizeNamespace(container); err != nil {
			return fmt.Errorf("finalize namespace %s", err)
		}
	*/

	// FinalizeNamespace can change user/group which clears the parent death
	// signal, so we restore it here.
	if err := RestoreParentDeathSignal(pdeathSignal); err != nil {
		return fmt.Errorf("restore parent death signal %s", err)
	}

	// Retain capabilities on clone.
	// TODO use the correct header file.
	secbit_keep_caps := 4
	secbit_no_setuid_fixup := 2
	if err := system.Prctl(syscall.PR_SET_SECUREBITS, uintptr(secbit_keep_caps|secbit_no_setuid_fixup), 0, 0, 0); err != nil {
		return fmt.Errorf("prctl %s", err)
	}

	// TODO: Pass the uid/gid from the caller.
	dockerRootUid := 1017
	dockerRootGid := 1017

	// Switch to the docker-root user.
	if err := system.Setuid(dockerRootUid); err != nil {
		return fmt.Errorf("setuid %s", err)
	}

	// Switch to the docker-root group.
	if err := system.Setgid(dockerRootGid); err != nil {
		return fmt.Errorf("setgid %s", err)
	}

	sPipe, err := NewSyncPipe()
	if err != nil {
		return err
	}

	pid, err := system.Clone(uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_FILES | syscall.SIGCHLD))
	if err != nil {
		return fmt.Errorf("userns clone: %s", err)
	}

	if pid != 0 {
		// In parent.
		log.Println("In parent.")
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}

		mappings := fmt.Sprintf("0 %v 1", dockerRootUid)
		if err = writeUserMappings(pid, mappings); err != nil {
			proc.Kill()
			return fmt.Errorf("Failed to write mappings: %s", err)
		}
		sPipe.Close()

		state, err := proc.Wait()
		if err != nil {
			proc.Kill()
			return fmt.Errorf("wait: %s", err)
		}

		os.Exit(state.Sys().(syscall.WaitStatus).ExitStatus())
	}

	// In child.
	log.Println("In child.")
	sPipe.Close()

	return syscall.Exec(args[0], args[0:], container.Env)
}

// Write UID/GID mappings for a process.
func writeUserMappings(pid int, mappings string) error {
	for _, p := range []string{
		fmt.Sprintf("/proc/%v/uid_map", pid),
		fmt.Sprintf("/proc/%v/gid_map", pid),
	} {
		if err := ioutil.WriteFile(p, []byte(mappings), 0644); err != nil {
			return err
		}
	}
	return nil
}

// RestoreParentDeathSignal sets the parent death signal to old.
func RestoreParentDeathSignal(old int) error {
	if old == 0 {
		return nil
	}

	current, err := system.GetParentDeathSignal()
	if err != nil {
		return fmt.Errorf("get parent death signal %s", err)
	}

	if old == current {
		return nil
	}

	if err := system.ParentDeathSignal(uintptr(old)); err != nil {
		return fmt.Errorf("set parent death signal %s", err)
	}

	// Signal self if parent is already dead. Does nothing if running in a new
	// PID namespace, as Getppid will always return 0.
	if syscall.Getppid() == 1 {
		return syscall.Kill(syscall.Getpid(), syscall.Signal(old))
	}

	return nil
}

// SetupUser changes the groups, gid, and uid for the user inside the container
func SetupUser(u string) error {
	uid, gid, suppGids, err := user.GetUserGroupSupplementary(u, syscall.Getuid(), syscall.Getgid())
	if err != nil {
		return fmt.Errorf("get supplementary groups %s", err)
	}
	if err := system.Setgroups(suppGids); err != nil {
		return fmt.Errorf("setgroups %s", err)
	}
	if err := system.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %s", err)
	}
	if err := system.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %s", err)
	}
	return nil
}

// setupVethNetwork uses the Network config if it is not nil to initialize
// the new veth interface inside the container for use by changing the name to eth0
// setting the MTU and IP address along with the default gateway
func setupNetwork(container *libcontainer.Container, context libcontainer.Context) error {
	for _, config := range container.Networks {
		strategy, err := network.GetStrategy(config.Type)
		if err != nil {
			return err
		}

		err1 := strategy.Initialize(config, context)
		if err1 != nil {
			return err1
		}
	}
	return nil
}

// FinalizeNamespace drops the caps, sets the correct user
// and working dir, and closes any leaky file descriptors
// before execing the command inside the namespace
func FinalizeNamespace(container *libcontainer.Container) error {
	/*
		if err := capabilities.DropCapabilities(container); err != nil {
			return fmt.Errorf("drop capabilities %s", err)
		}
	*/
	if err := system.CloseFdsFrom(3); err != nil {
		return fmt.Errorf("close open file descriptors %s", err)
	}
	if err := SetupUser(container.User); err != nil {
		return fmt.Errorf("setup user %s", err)
	}
	if container.WorkingDir != "" {
		if err := system.Chdir(container.WorkingDir); err != nil {
			return fmt.Errorf("chdir to %s %s", container.WorkingDir, err)
		}
	}
	return nil
}

func LoadContainerEnvironment(container *libcontainer.Container) error {
	os.Clearenv()
	for _, pair := range container.Env {
		p := strings.SplitN(pair, "=", 2)
		if len(p) < 2 {
			return fmt.Errorf("invalid environment '%v'", pair)
		}
		if err := os.Setenv(p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}
