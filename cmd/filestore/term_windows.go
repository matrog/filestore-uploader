//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// enableVT turns on ANSI escape handling in Windows consoles that do not do it
// themselves (classic cmd.exe). Windows Terminal does not need it, but the call
// is harmless there.
func enableVT() {
	const enableVirtualTerminalProcessing = 0x0004

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	handle := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if r, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return
	}
	setConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
}

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

func termCols() int {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetConsoleScreenBufferInfo")

	var info consoleScreenBufferInfo
	r, _, _ := proc.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0
	}
	if w := int(info.Window.Right-info.Window.Left) + 1; w > 0 {
		return w
	}
	return 0
}
