@echo off
REM build.cmd —— Windows 下的构建入口
REM 委托给 build.sh 执行（自动递增版本号尾数）。
REM 需要 Git Bash；若未安装，请直接使用：
REM   go build -trimpath -ldflags "-s -w" -o pngopt.exe .

setlocal

where bash >nul 2>nul
if errorlevel 1 (
	echo 未找到 bash，请安装 Git for Windows 或使用 MSYS2。
	echo 或直接执行：go build -trimpath -ldflags "-s -w" -o pngopt.exe .
	exit /b 1
)

bash "%~dp0build.sh" %*
exit /b %errorlevel%
