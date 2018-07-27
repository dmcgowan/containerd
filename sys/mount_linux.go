/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package sys

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/pkg/errors"
)

// Fmountat performs the mount from the provided directory file descriptor
func Fmountat(fd uintptr, source string, target string, fstype string, flags uintptr, data string) (err error) {
	// Do Go variable allocation before clone to prevent stack growth
	// after clone.
	var (
		sourceP, targetP, fstypeP *byte
		dataP                     *byte
		errno                     syscall.Errno
	)
	sourceP, err = syscall.BytePtrFromString(source)
	if err != nil {
		return
	}
	targetP, err = syscall.BytePtrFromString(target)
	if err != nil {
		return
	}
	fstypeP, err = syscall.BytePtrFromString(fstype)
	if err != nil {
		return
	}
	if data != "" {
		dataP, err = syscall.BytePtrFromString(data)
		if err != nil {
			return
		}
	}

	runtime.LockOSThread()

	beforeFork()
	pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, uintptr(syscall.SIGCHLD)|syscall.CLONE_FILES, 0, 0, 0, 0, 0)
	if errno != 0 {
		afterFork()

		runtime.UnlockOSThread()

		return errors.Errorf("clone failed with %d", errno)
	}

	// if in the parent, wait on the child mount to finish and convert error
	if pid != 0 {
		afterFork()

		runtime.UnlockOSThread()

		p, err := os.FindProcess(int(pid))
		if err != nil {
			return err
		}

		ps, err := p.Wait()
		if err != nil {
			return err
		}

		if !ps.Success() {
			ws := ps.Sys().(syscall.WaitStatus)
			errno := ws.ExitStatus()
			if errno >= 0x7F {
				return errors.Wrap(syscall.Errno(errno-0x7F), "chdir")
			}
			return errors.Wrap(syscall.Errno(errno), "mount")

		}

		return nil
	}
	afterForkInChild()

	_, _, errno = syscall.RawSyscall(syscall.SYS_FCHDIR, fd, 0, 0)
	if errno != 0 {
		if errno < 0x7F {
			errno += 0x7F
		}
		syscall.RawSyscall(syscall.SYS_EXIT, uintptr(errno), 0, 0)
	}

	_, _, errno = syscall.Syscall6(syscall.SYS_MOUNT, uintptr(unsafe.Pointer(sourceP)), uintptr(unsafe.Pointer(targetP)), uintptr(unsafe.Pointer(fstypeP)), uintptr(flags), uintptr(unsafe.Pointer(dataP)), 0)

	syscall.RawSyscall(syscall.SYS_CLOSE, fd, 0, 0)
	syscall.RawSyscall(syscall.SYS_EXIT, uintptr(errno), 0, 0)

	panic("unreachable")
}
