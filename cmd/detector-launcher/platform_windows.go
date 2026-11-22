//go:build windows

package main

import (
	"encoding/binary"
	"errors"
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

var tcpTable = systemProc("iphlpapi.dll", "GetExtendedTcpTable")

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

func namedMutex(name string) (syscall.Handle, bool, error) {
	value, _ := syscall.UTF16PtrFromString(name)
	h, _, err := kernel.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(value)))
	if h == 0 {
		return 0, false, err
	}
	return syscall.Handle(h), err == syscall.ERROR_ALREADY_EXISTS, nil
}
func markerExists(name string) bool {
	value, _ := syscall.UTF16PtrFromString(name)
	h, _, _ := kernel.NewProc("OpenMutexW").Call(syscall.SYNCHRONIZE, 0, uintptr(unsafe.Pointer(value)))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}

// Kill-on-close ties the SSH child's lifetime to the native launcher, including an unexpected exit.
func ownChild(pid int) (syscall.Handle, error) {
	job, _, err := kernel.NewProc("CreateJobObjectW").Call(0, 0)
	if job == 0 {
		return 0, err
	}
	type basicLimits struct {
		perProcessTime, perJobTime   int64
		flags                        uint32
		minWorkingSet, maxWorkingSet uintptr
		activeProcesses              uint32
		affinity                     uintptr
		priority, scheduling         uint32
	}
	var limits struct {
		basic  basicLimits
		io     [6]uint64
		memory [4]uintptr
	}
	limits.basic.flags = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ok, _, err := kernel.NewProc("SetInformationJobObject").Call(job, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if ok != 0 {
		var proc syscall.Handle
		proc, err = syscall.OpenProcess(0x100|syscall.PROCESS_TERMINATE, false, uint32(pid))
		if err == nil {
			ok, _, err = kernel.NewProc("AssignProcessToJobObject").Call(job, uintptr(proc))
			syscall.CloseHandle(proc)
			if ok != 0 {
				return syscall.Handle(job), nil
			}
		}
	}
	syscall.CloseHandle(syscall.Handle(job))
	return 0, fmt.Errorf("无法管理 SSH 子进程：%w", err)
}

type tcpState struct {
	pid       uint32
	connected bool
	peers     map[uint16]bool
}

func portState(port uint16) (tcpState, error) {
	var state tcpState
	state.peers = make(map[uint16]bool)
	var size uint32 = 16384
	for {
		buf := make([]byte, size)
		result, _, _ := tcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 2, 5, 0) // AF_INET, TCP_TABLE_OWNER_PID_ALL
		if result == 122 {
			continue
		}
		if result != 0 {
			return state, syscall.Errno(result)
		}
		// MIB_TCPTABLE_OWNER_PID starts with a row count, followed by six DWORDs per row.
		rows := int(binary.LittleEndian.Uint32(buf[:4]))
		if rows > (len(buf)-4)/24 {
			return state, errors.New("Windows TCP 状态表不完整。")
		}
		for i := 0; i < rows; i++ {
			row := buf[4+i*24:]
			localPort := binary.BigEndian.Uint16(row[8:10])
			remotePort := binary.BigEndian.Uint16(row[16:18])
			if localPort != port && remotePort != port {
				continue
			}
			status := binary.LittleEndian.Uint32(row[:4])
			host := binary.BigEndian.Uint32(row[4:8])
			if status == 2 && (host == 0x7f000001 || host == 0) {
				if host == 0 || state.pid != 0 {
					return state, portBusy
				}
				state.pid = binary.LittleEndian.Uint32(row[20:24])
				continue
			}
			if host != 0x7f000001 || binary.BigEndian.Uint32(row[12:16]) != 0x7f000001 {
				continue
			}
			if localPort == port {
				state.peers[remotePort] = true
			} else {
				// A short request's TIME_WAIT entry can be on the browser side.
				state.peers[localPort] = true
			}
			if status == 5 {
				state.connected = true
			}
		}
		return state, nil
	}
}
