//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// enableVirtualTerminalProcessing 是 Windows 控制台启用 ANSI 转义序列的标志位。
//
// Go 标准库不会自动开启它，导致 \033[K（清除行）等序列在传统 cmd.exe 中
// 被当作普通字符打印出来，进度条表现为乱码且不换行。
const enableVirtualTerminalProcessing = 0x0004

// isTerminal 判断文件是否连接到控制台（而非重定向到文件或管道）。
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// enableANSI 尝试为控制台开启虚拟终端处理（ANSI 转义序列支持）。
// 返回是否成功启用；失败时调用方应退回到无转义序列的输出方式。
func enableANSI(f *os.File) bool {
	if f == nil {
		return false
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true // 已经启用
	}
	r, _, _ = procSetConsoleMode.Call(f.Fd(), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
