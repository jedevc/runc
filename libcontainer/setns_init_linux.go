package libcontainer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/opencontainers/selinux/go-selinux"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/opencontainers/runc/libcontainer/apparmor"
	"github.com/opencontainers/runc/libcontainer/keys"
	"github.com/opencontainers/runc/libcontainer/seccomp"
	"github.com/opencontainers/runc/libcontainer/system"
)

// linuxSetnsInit performs the container's initialization for running a new process
// inside an existing container.
type linuxSetnsInit struct {
	pipe          *os.File
	consoleSocket *os.File
	config        *initConfig
	logFd         int
	dmzExe        *os.File
}

func (l *linuxSetnsInit) getSessionRingName() string {
	return "_ses." + l.config.ContainerID
}

func (l *linuxSetnsInit) Init() error {
	if !l.config.Config.NoNewKeyring {
		if err := selinux.SetKeyLabel(l.config.ProcessLabel); err != nil {
			return err
		}
		defer selinux.SetKeyLabel("") //nolint: errcheck
		// Do not inherit the parent's session keyring.
		if _, err := keys.JoinSessionKeyring(l.getSessionRingName()); err != nil {
			// Same justification as in standart_init_linux.go as to why we
			// don't bail on ENOSYS.
			//
			// TODO(cyphar): And we should have logging here too.
			if !errors.Is(err, unix.ENOSYS) {
				return fmt.Errorf("unable to join session keyring: %w", err)
			}
		}
	}
	if l.config.CreateConsole {
		if err := setupConsole(l.consoleSocket, l.config, false); err != nil {
			return err
		}
		if err := system.Setctty(); err != nil {
			return err
		}
	}
	if l.config.NoNewPrivileges {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return err
		}
	}
	if l.config.Config.Umask != nil {
		unix.Umask(int(*l.config.Config.Umask))
	}

	if err := selinux.SetExecLabel(l.config.ProcessLabel); err != nil {
		return err
	}
	defer selinux.SetExecLabel("") //nolint: errcheck
	// Without NoNewPrivileges seccomp is a privileged operation, so we need to
	// do this before dropping capabilities; otherwise do it as late as possible
	// just before execve so as few syscalls take place after it as possible.
	if l.config.Config.Seccomp != nil && !l.config.NoNewPrivileges {
		seccompFd, err := seccomp.InitSeccomp(l.config.Config.Seccomp)
		if err != nil {
			return err
		}
		if err := syncParentSeccomp(l.pipe, seccompFd); err != nil {
			return err
		}
	}
	if err := finalizeNamespace(l.config); err != nil {
		return err
	}
	if err := apparmor.ApplyProfile(l.config.AppArmorProfile); err != nil {
		return err
	}
	// Check for the arg early to make sure it exists.
	name, err := exec.LookPath(l.config.Args[0])
	if err != nil {
		return err
	}
	// exec.LookPath in Go < 1.20 might return no error for an executable
	// residing on a file system mounted with noexec flag, so perform this
	// extra check now while we can still return a proper error.
	// TODO: remove this once go < 1.20 is not supported.
	if err := eaccess(name); err != nil {
		return &os.PathError{Op: "eaccess", Path: name, Err: err}
	}
	// Set seccomp as close to execve as possible, so as few syscalls take
	// place afterward (reducing the amount of syscalls that users need to
	// enable in their seccomp profiles).
	if l.config.Config.Seccomp != nil && l.config.NoNewPrivileges {
		seccompFd, err := seccomp.InitSeccomp(l.config.Config.Seccomp)
		if err != nil {
			return fmt.Errorf("unable to init seccomp: %w", err)
		}
		if err := syncParentSeccomp(l.pipe, seccompFd); err != nil {
			return err
		}
	}
	logrus.Debugf("setns_init: about to exec")

	// Close the log pipe fd so the parent's ForwardLogs can exit.
	if err := unix.Close(l.logFd); err != nil {
		return &os.PathError{Op: "close log pipe", Path: "fd " + strconv.Itoa(l.logFd), Err: err}
	}

	if l.dmzExe != nil {
		l.config.Args[0] = name
		return system.Fexecve(l.dmzExe.Fd(), l.config.Args, os.Environ())
	}
	return system.Exec(name, l.config.Args, os.Environ())
}
