//go:build windows

package setup

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const moveFileReplaceExisting = 0x1

var (
	kernel32   = syscall.NewLazyDLL("kernel32.dll")
	moveFileEx = kernel32.NewProc("MoveFileExW")
)

func replaceFile(source, destination string) error {
	src, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	dst, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	ok, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(dst)), moveFileReplaceExisting)
	if ok == 0 {
		if callErr != nil && !errors.Is(callErr, syscall.Errno(0)) {
			return callErr
		}
		return os.ErrInvalid
	}
	return nil
}
