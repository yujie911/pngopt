//go:build !windows

package main

import "os"

// isTerminal 判断文件是否连接到终端。
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// enableANSI 在非 Windows 平台上无需额外处理，终端原生支持 ANSI 转义序列。
func enableANSI(f *os.File) bool {
	return isTerminal(f)
}
