//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")

func systemDir() (string, error) {
	var out [32768]uint16
	n, _, err := kernel.NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)))
	if n == 0 || n >= uintptr(len(out)) {
		return "", err
	}
	return syscall.UTF16ToString(out[:n]), nil
}

func systemProc(dll, proc string) *syscall.LazyProc {
	dir, err := systemDir()
	if err != nil {
		panic(err)
	}
	return syscall.NewLazyDLL(filepath.Join(dir, dll)).NewProc(proc)
}

func launchBrowser(url string) error {
	u, _ := syscall.UTF16PtrFromString(url)
	value, _, _ := systemProc("shell32.dll", "ShellExecuteW").Call(0, 0, uintptr(unsafe.Pointer(u)), 0, 0, 1)
	if value <= 32 {
		return fmt.Errorf("无法启动默认浏览器，请打开 %s（Windows 代码 %d）。", url, value)
	}
	return nil
}
func showError(message string) {
	text, _ := syscall.UTF16PtrFromString(message)
	title, _ := syscall.UTF16PtrFromString("账号指纹检测")
	systemProc("user32.dll", "MessageBoxW").Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}
