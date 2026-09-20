//go:build !windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Su Unix le sequenze ANSI funzionano senza preparativi.
func enableVT() {}

type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// termCols chiede al kernel la larghezza reale del terminale. La variabile
// COLUMNS non basta: le shell non la esportano ai processi figli.
func termCols() int {
	var ws winsize
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}
