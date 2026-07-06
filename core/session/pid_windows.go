//go:build windows

package session

import "syscall"

const (
	windowsSynchronize       = 0x00100000
	windowsWaitObject0       = 0x00000000
	windowsWaitTimeout       = 0x00000102
	windowsErrorAccessDenied = syscall.Errno(5)
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess         = kernel32.NewProc("OpenProcess")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
)

func pidRunning(pid int) (bool, error) {
	handle, _, err := procOpenProcess.Call(windowsSynchronize, 0, uintptr(uint32(pid)))
	if handle == 0 {
		if err == windowsErrorAccessDenied {
			return true, nil
		}
		return false, nil
	}
	defer procCloseHandle.Call(handle)

	result, _, waitErr := procWaitForSingleObject.Call(handle, 0)
	switch result {
	case windowsWaitTimeout:
		return true, nil
	case windowsWaitObject0:
		return false, nil
	default:
		if waitErr != syscall.Errno(0) {
			return true, waitErr
		}
		return true, nil
	}
}
