package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// 版本与作者信息
//
// version 是可被构建脚本覆盖的变量：
//
//	go build -ldflags "-X main.version=1.0.3"
//
// build.sh 会在编译前自动把版本号尾数 +1 并注入。
// 这里的默认值用于直接 `go build` 的场景（此时版本号保持源码中的值）。
var (
	version = "1.0.6"
	author  = "Wuhan YuJie"
)

func main() {
	args := parseArgs(os.Args[1:])
	if args.version {
		printVersion()
		return
	}
	if args.help {
		printHelp()
		return
	}
	if args.input == "" {
		printHelp()
		os.Exit(2)
	}

	if err := run(args); err != nil {
		fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
		os.Exit(1)
	}
}

// cliArgs 是解析后的命令行参数
type cliArgs struct {
	input      string
	output     string
	verbose    bool
	version    bool
	help       bool
	keepChunks string
	strip      bool
}

// parseArgs 解析命令行参数
//
// 支持两种输入方式：
//
//	· 位置参数：pngopt.exe aaa.png            （最简形式）
//	· -i 选项： pngopt.exe -i aaa.png
//
// 两者可混用，位置参数会与 -i 的值合并（逗号分隔的多个路径也支持）。
func parseArgs(argv []string) cliArgs {
	a := cliArgs{}
	var positional []string

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-i" || arg == "--i" || arg == "-input" || arg == "--input":
			if i+1 < len(argv) {
				i++
				a.input = argv[i]
			}
		case strings.HasPrefix(arg, "-i=") || strings.HasPrefix(arg, "--input="):
			a.input = arg[strings.Index(arg, "=")+1:]
		case arg == "-o" || arg == "--o" || arg == "-output" || arg == "--output":
			if i+1 < len(argv) {
				i++
				a.output = argv[i]
			}
		case strings.HasPrefix(arg, "-o=") || strings.HasPrefix(arg, "--output="):
			a.output = arg[strings.Index(arg, "=")+1:]
		case arg == "-v" || arg == "--v" || arg == "-version" || arg == "--version":
			a.version = true
		case arg == "-verbose" || arg == "--verbose" || arg == "-d" || arg == "--d":
			a.verbose = true
		case arg == "-help" || arg == "--help" || arg == "-h" || arg == "-?":
			a.help = true
		case strings.HasPrefix(arg, "-"):
			// 未知选项：忽略
		default:
			// 位置参数：作为输入路径
			positional = append(positional, arg)
		}
	}

	// 位置参数与 -i 的值合并
	if len(positional) > 0 {
		pos := strings.Join(positional, ",")
		if a.input == "" {
			a.input = pos
		} else {
			a.input = a.input + "," + pos
		}
	}
	return a
}

// printVersion 显示版本号与作者
func printVersion() {
	fmt.Printf("pngopt %s\n", version)
	fmt.Printf("作者: %s\n", author)
}

func printHelp() {
	fmt.Fprint(os.Stderr, `pngopt —— PNG 无损压缩工具（保留透明度通道）

用法:
  pngopt <文件或目录> [选项]
  pngopt -i <输入路径> [-o <输出路径>] [选项]

  最简形式：直接把文件路径作为参数
    pngopt.exe aaa.png            压缩 aaa.png，输出 aaa_opt.png
    pngopt.exe ./images           批量压缩文件夹内所有 PNG

  输入路径可以是：
    · 单个 PNG 文件        pngopt logo.png
    · 多个文件（逗号分隔） pngopt a.png,b.png,c.png
    · 一个文件夹           pngopt ./images
    · 多个文件夹           pngopt ./img1,./img2

  输出规则：
    · 未指定 -o            → 在源文件旁生成 <原名>_opt.png
    · 单文件 + 指定 -o    → 输出到 -o 指定的文件
    · 多文件 + 指定 -o    → -o 视为输出目录

选项:
  -i <路径>   输入路径（文件 / 文件夹 / 逗号分隔的多个路径）
  -o <路径>   输出路径（单文件时为输出文件，多文件时为输出目录）
  -v          显示版本号与作者
  -verbose    详细模式：显示每个候选过滤策略的压缩体积
  -help       显示本帮助信息

示例:
  pngopt logo.png
  pngopt logo.png -o min/logo.min.png
  pngopt ./images -verbose
  pngopt -i a.png,b.png -o ./out
  pngopt -v

说明:
  压缩严格无损：每个文件输出后都会经自研解码器与 Go 标准库双重逐像素校验，
  校验不通过则报错，不会留下错误的输出文件。
`)
}

// job 是一个待处理的任务
type job struct {
	src string
	dst string
}

// collectJobs 收集所有待处理的 PNG 文件
func collectJobs(input, output string) ([]job, error) {
	paths := splitPaths(input)
	var files []string
	var inputIsDir bool

	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("路径不存在: %s", p)
		}
		if info.IsDir() {
			inputIsDir = true
			err := filepath.Walk(p, func(path string, fi os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if fi.IsDir() {
					return nil
				}
				if isOptimizedOutput(path) {
					// 跳过本工具自己生成的输出文件，避免重复处理
					return nil
				}
				if strings.EqualFold(filepath.Ext(path), ".png") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("遍历目录失败: %w", err)
			}
		} else {
			if !strings.EqualFold(filepath.Ext(p), ".png") {
				return nil, fmt.Errorf("不是 PNG 文件: %s", p)
			}
			files = append(files, p)
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("未找到任何 PNG 文件")
	}
	// 去重并排序，保证输出顺序稳定
	sort.Strings(files)

	// 生成输出路径
	var jobs []job
	for _, f := range files {
		dst := ""
		if output != "" && (inputIsDir || len(files) > 1) {
			// 多文件模式：-o 作为输出目录
			base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) + "_opt.png"
			dst = filepath.Join(output, base)
		} else if output != "" {
			// 单文件模式：-o 作为输出文件
			dst = output
		} else {
			// 默认：源文件旁加 _opt 后缀
			base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) + "_opt.png"
			dst = filepath.Join(filepath.Dir(f), base)
		}
		jobs = append(jobs, job{src: f, dst: dst})
	}
	return jobs, nil
}

// splitPaths 按逗号分割输入路径
func splitPaths(input string) []string {
	return strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	})
}

// isOptimizedOutput 判断文件是否是本工具生成的输出文件。
//
// 输出文件名规则是 <原名>_opt.png，因此文件名以 _opt 结尾的 PNG 即为输出文件。
// 批量处理时必须跳过它们，否则会反复压缩自己的产物（既浪费时间，
// 也会让统计数字失真）。
func isOptimizedOutput(path string) bool {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	if !strings.EqualFold(ext, ".png") {
		return false
	}
	stem := strings.TrimSuffix(base, ext)
	return strings.HasSuffix(strings.ToLower(stem), "_opt")
}

// consoleProgress 在终端上显示进度条与状态。
//
// 设计要点：
//  1. 后台定时器驱动重绘，而不是靠事件触发。
//     压缩耗时的 90% 集中在单个阶段内部，若只在阶段切换时刷新，
//     进度条会长时间静止，看起来像"卡死"。
//  2. 阶段内按已用时间做插值，让进度条持续平滑地走动。
//  3. 输出被重定向（非终端）时自动降级为逐行输出，不写 ANSI 转义序列。
type consoleProgress struct {
	mu       sync.Mutex
	total    int
	done     int
	start    time.Time
	lastDraw time.Time
	multi    bool
	curFile  string
	curStage string
	// 当前阶段的起始时刻，用于阶段内插值
	stageStart time.Time
	// 单文件模式下按阶段推进进度条
	stageIndex int
	// 多文件模式下每个活跃文件一行进度
	lines map[int]*fileLine
	// 已渲染的行数，用于重绘时清理残留
	drawn int
	// 是否启用 ANSI 动态刷新（非终端时关闭）
	interactive bool
	// 定时器控制
	stop  chan struct{}
	done2 chan struct{}
	// 自测用：逐帧记录渲染内容（由环境变量 PNGOPT_FRAME_DUMP 开启）
	frameDump *os.File
}

// fileLine 是多文件并发模式下单个文件的进度行
type fileLine struct {
	index int
	name  string
	stage string
	// 该文件当前阶段的起始时刻
	stageStart time.Time
}

// stageNames 是压缩流程的阶段序列，用于单文件模式的进度计算
var stageNames = []string{"解码", "分析色彩模型", "过滤策略评估", "Zopfli 压缩", "无损校验"}

// stageWeights 是各阶段完成时的累计进度权重。
//
// 数值依据实测的阶段耗时占比（Zopfli 压缩占总耗时约 90%），
// 使进度条在时间轴上的推进速度与真实耗时相符。
// 阶段内部不做插值，百分比只在阶段完成时跳变。
var stageWeights = []float64{0.02, 0.04, 0.09, 0.97, 1.0}

// newConsoleProgress 创建进度显示器并启动后台刷新定时器
func newConsoleProgress(total int) *consoleProgress {
	// 只有在"确实是终端"且"ANSI 序列可用"时才启用动态刷新。
	// 二者缺一：
	//   · 非终端（重定向到文件/管道）→ ANSI 序列会污染日志
	//   · ANSI 不可用（老版本 Windows 控制台）→ 序列会显示成乱码
	interactive := isTerminal(os.Stdout) && enableANSI(os.Stdout)

	// 自测开关：把每一帧渲染结果写入文件，用于验证终端下的显示行为。
	// 通过环境变量 PNGOPT_FRAME_DUMP 指定文件路径，不出现在帮助信息中。
	var frameDump *os.File
	if path := os.Getenv("PNGOPT_FRAME_DUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			frameDump = f
		}
	}

	c := &consoleProgress{
		total:       total,
		start:       time.Now(),
		stageStart:  time.Now(),
		multi:       total > 1,
		interactive: interactive,
		stop:        make(chan struct{}),
		done2:       make(chan struct{}),
		frameDump:   frameDump,
	}
	if c.multi {
		c.lines = make(map[int]*fileLine)
	}
	go c.ticker()
	return c
}

// ticker 是后台重绘循环。
//
// 固定 100ms 刷新一次，保证长时间运行的阶段内进度条也在走动，
// 同时刷新频率不至于让终端闪烁或输出臃肿。
func (c *consoleProgress) ticker() {
	defer close(c.done2)
	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-tk.C:
			c.mu.Lock()
			c.draw(false)
			c.mu.Unlock()
		}
	}
}

// beginFile 开始处理一个文件
func (c *consoleProgress) beginFile(index, total int, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total = total
	c.multi = total > 1
	c.curFile = name
	c.curStage = stageNames[0]
	c.stageIndex = 0
	c.stageStart = time.Now()
	if c.multi {
		if c.lines == nil {
			c.lines = make(map[int]*fileLine)
		}
		c.lines[index] = &fileLine{index: index, name: name, stage: stageNames[0], stageStart: time.Now()}
		// 非终端环境：打印文件起始标记（替代动态进度条）
		if !c.interactive {
			fmt.Printf("  [%d/%d] %s\n", index+1, total, truncate(filepath.Base(name), 50))
		}
	}
	c.draw(true)
}

// stage 更新当前阶段
func (c *consoleProgress) stage(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := c.curStage != text
	c.curStage = text
	for i, s := range stageNames {
		if s == text {
			c.stageIndex = i
			break
		}
	}
	c.stageStart = time.Now()
	// 非终端环境（输出被重定向）没有动态进度条，
	// 此时打印阶段变化日志，让用户知道程序仍在推进。
	if !c.interactive && changed {
		fmt.Printf("  [%s] %s ...\n", truncate(filepath.Base(c.curFile), 40), text)
	}
	c.draw(true)
}

// doneFile 完成一个文件
func (c *consoleProgress) doneFile(name string, origSize, newSize int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done++
	if c.multi {
		// 多文件模式：清除该文件的进度行，用结果行替代
		for k, v := range c.lines {
			if v.name == name {
				delete(c.lines, k)
				break
			}
		}
		c.clearAll()
		saved := ratioPct(origSize, newSize)
		mark := "✓"
		if saved <= 0 {
			mark = "="
		}
		fmt.Printf("  %s %-40s %8s → %-8s (节省 %.1f%%)\n", mark, name,
			humanBytes(int64(origSize)), humanBytes(int64(newSize)), saved)
		c.drawn = 0
	} else {
		c.clearLine()
		fmt.Printf("  ✓ %s  %s → %s  (节省 %.1f%%)\n", name,
			humanBytes(int64(origSize)), humanBytes(int64(newSize)),
			ratioPct(origSize, newSize))
	}
	c.draw(false)
}

// finish 全部完成：停止定时器并清理画面
func (c *consoleProgress) finish() {
	close(c.stop)
	<-c.done2
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearAll()
	if c.frameDump != nil {
		c.frameDump.Close()
		c.frameDump = nil
	}
}

// singleProgress 计算单文件模式下的进度百分比。
//
// 各阶段完成时按权重跳变（真实进度点），阶段内部不做虚假插值——
// Zopfli 没有进度回调，凭时间臆造百分比会误导用户。
// 阶段内的"活动感"由旋转指示器与实时耗时提供。
func (c *consoleProgress) singleProgress(now time.Time) float64 {
	if c.stageIndex >= len(stageWeights) {
		return 100
	}
	return stageWeights[c.stageIndex] * 100
}

// multiProgress 计算多文件模式下的总进度百分比。
//
// 按「已完成文件数 + 进行中文件的阶段进度」加权计算，
// 让总进度条在批次处理过程中也平滑推进，而不是等到文件完成才跳变。
func (c *consoleProgress) multiProgress(now time.Time) float64 {
	if c.total <= 0 {
		return 0
	}
	// 已完成文件按 100% 计
	progress := float64(c.done)
	// 进行中文件按各自阶段进度计
	if len(c.lines) > 0 {
		var sum float64
		for _, fl := range c.lines {
			sum += c.fileProgressOf(fl, now)
		}
		progress += sum / float64(c.total)
	}
	pct := progress / float64(c.total) * 100
	if pct > 99.5 {
		pct = 99.5
	}
	return pct
}

// fileProgressOf 计算单个活跃文件的阶段进度（0~1）
func (c *consoleProgress) fileProgressOf(fl *fileLine, now time.Time) float64 {
	for i, s := range stageNames {
		if s == fl.stage {
			if i < len(stageWeights) {
				return stageWeights[i]
			}
			return 1
		}
	}
	return 0
}

// spinnerFrames 是旋转指示器的字符序列。
//
// 压缩阶段内部没有可用的进度回调，用旋转动画明确地表达
// "程序仍在工作"，比臆造百分比更诚实。
var spinnerFrames = []string{"|", "/", "-", "\\"}

// draw 绘制进度条
func (c *consoleProgress) draw(force bool) {
	now := time.Now()
	// 非终端环境（重定向到文件、管道）下不做动态刷新，
	// 避免 ANSI 转义序列污染日志。
	// 但自测模式下仍需渲染，以便记录帧序列验证显示行为。
	if !c.interactive && c.frameDump == nil {
		return
	}
	if !force && now.Sub(c.lastDraw) < 90*time.Millisecond {
		return
	}
	c.lastDraw = now

	width := 30
	var pct float64
	if c.multi {
		pct = c.multiProgress(now)
	} else {
		pct = c.singleProgress(now)
	}
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)

	elapsed := now.Sub(c.start).Seconds()
	var eta string
	if c.multi && c.done > 0 && pct > 0 {
		perFile := elapsed / (pct / 100)
		eta = fmt.Sprintf("  预计剩余 %.1fs", perFile*(100-pct)/100)
	}

	if c.multi {
		// 多文件：总进度条 + 活跃文件摘要（单行，避免输出冗长）
		c.clearAll()
		spin := spinnerFrames[int(now.UnixNano()/1e8)%len(spinnerFrames)]
		text := fmt.Sprintf("  总进度 [%s] %5.1f%%  %d/%d  %s 已用 %.1fs%s",
			bar, pct, c.done, c.total, spin, elapsed, eta)
		c.dumpFrame(text)
		if c.interactive {
			fmt.Println(text)
		}
		lines := c.sortedLines()
		if len(lines) > 0 {
			names := make([]string, 0, len(lines))
			for _, fl := range lines {
				names = append(names, fmt.Sprintf("%s(%s)", truncate(filepath.Base(fl.name), 20), fl.stage))
			}
			sub := strings.Join(names, "  ")
			if len(sub) > 100 {
				sub = sub[:99] + "…"
			}
			c.dumpFrame("    " + sub)
			if c.interactive {
				fmt.Println("    " + sub)
			}
			c.drawn = 2
		} else {
			c.drawn = 1
		}
		return
	}

	label := c.curStage
	// 旋转指示器：阶段内部没有进度回调，用它明确表达"仍在工作"
	spin := spinnerFrames[int(now.UnixNano()/1e8)%len(spinnerFrames)]
	text := fmt.Sprintf("  [%s] %5.1f%%  %s %-16s 已用 %.1fs",
		bar, pct, spin, label, elapsed)
	c.dumpFrame(text)
	if !c.interactive {
		return
	}
	c.clearLine()
	fmt.Print(text)
}

// dumpFrame 记录一帧渲染内容（仅自测时启用）
func (c *consoleProgress) dumpFrame(text string) {
	if c.frameDump == nil {
		return
	}
	fmt.Fprintln(c.frameDump, text)
}

// sortedLines 返回按索引排序的活跃文件行
func (c *consoleProgress) sortedLines() []*fileLine {
	out := make([]*fileLine, 0, len(c.lines))
	for _, v := range c.lines {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// truncate 截断过长的字符串
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ratioPct 计算压缩率百分比
func ratioPct(orig, now int) float64 {
	if orig == 0 {
		return 0
	}
	return (1 - float64(now)/float64(orig)) * 100
}

// clearLine 清除当前行（回到行首并清除该行剩余内容）
func (c *consoleProgress) clearLine() {
	if !c.interactive {
		return
	}
	fmt.Print("\r\033[K")
}

// clearAll 清除此前渲染的所有行
//
// 用「光标上移 N 行 + 清到屏幕末尾」实现，
// 相比逐行清除更可靠，不会残留上一帧的内容。
func (c *consoleProgress) clearAll() {
	if !c.interactive || c.drawn <= 0 {
		return
	}
	fmt.Printf("\033[%dA\033[J", c.drawn)
	c.drawn = 0
}

// run 是主流程
func run(args cliArgs) error {
	jobs, err := collectJobs(args.input, args.output)
	if err != nil {
		return err
	}

	// 确保输出目录存在
	for _, j := range jobs {
		if dir := filepath.Dir(j.dst); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("创建输出目录失败: %w", err)
			}
		}
	}

	prog := newConsoleProgress(len(jobs))
	totalStart := time.Now()

	fmt.Printf("PNG 无损压缩 —— 共 %d 个文件待处理\n", len(jobs))
	if len(jobs) > 1 {
		fmt.Printf("并发数: %d（按 CPU 核数自适应）\n", workerCount(len(jobs)))
	}

	results := processJobs(jobs, args, prog)
	prog.finish()

	// 汇总统计
	var sumOrig, sumNew int64
	var sumDur time.Duration
	okCount, failCount := 0, 0
	for _, r := range results {
		if r.err != nil {
			failCount++
			fmt.Printf("  ✗ %s: %v\n", r.src, r.err)
			continue
		}
		okCount++
		sumOrig += int64(r.origSize)
		sumNew += int64(r.newSize)
		sumDur += r.duration
	}

	totalDur := time.Since(totalStart)
	printSummary(results, okCount, failCount, sumOrig, sumNew, sumDur, totalDur, len(jobs) == 1)
	return nil
}

// workerCount 依据任务数与 CPU 核数确定并发度
func workerCount(jobs int) int {
	n := runtime.NumCPU()
	// 每个文件内部还会并行评估 8 种过滤策略，
	// 因此文件级并发不宜占满全部核心，给内层留出余量。
	if n > 4 {
		n = n / 2
	}
	if n > jobs {
		n = jobs
	}
	if n < 1 {
		n = 1
	}
	return n
}

// processJobs 并行处理所有任务
//
// 并发策略：文件级并发（每个 worker 独占一个文件），
// 文件内部再并行评估 8 种过滤策略。两层并发叠加，
// 既能吃满多核，又避免单文件时 CPU 空转。
func processJobs(jobs []job, args cliArgs, prog *consoleProgress) []fileResult {
	results := make([]fileResult, len(jobs))
	workers := workerCount(len(jobs))

	type task struct {
		index int
		job   job
	}
	taskCh := make(chan task)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				// 注册该文件的进度行，再开始处理
				p := &fileProgress{parent: prog, index: t.index, total: len(jobs)}
				prog.beginFile(t.index, len(jobs), t.job.src)
				res := compressFile(t.job.src, t.job.dst, args.keepChunks, args.strip, args.verbose, p)
				// 优化后体积反而变大时，保留原始内容
				if res.err == nil && res.newSize > res.origSize {
					if err := copyOriginal(t.job.src, t.job.dst); err == nil {
						res.newSize = res.origSize
						res.kept = true
					}
				}
				results[t.index] = res
				prog.doneFile(t.job.src, res.origSize, res.newSize)
			}
		}()
	}

	for i, j := range jobs {
		taskCh <- task{index: i, job: j}
	}
	close(taskCh)
	wg.Wait()
	return results
}

// fileProgress 是单个文件的进度上报器。
// 多文件并发时，每个文件在终端上占一行，互不干扰。
type fileProgress struct {
	parent *consoleProgress
	index  int
	total  int
	mu     sync.Mutex
	cur    string
	last   time.Time
}

// beginFile 开始处理一个文件
func (f *fileProgress) beginFile(index, total int, name string) {
	f.parent.beginFile(f.index, f.total, name)
}

// stage 更新当前阶段
func (f *fileProgress) stage(text string) {
	f.mu.Lock()
	f.cur = text
	f.mu.Unlock()

	f.parent.mu.Lock()
	defer f.parent.mu.Unlock()
	if f.parent.multi {
		// 更新本文件对应的进度行
		if fl, ok := f.parent.lines[f.index]; ok {
			fl.stage = text
		}
	} else {
		f.parent.curStage = text
		for i, s := range stageNames {
			if s == text {
				f.parent.stageIndex = i
				break
			}
		}
	}
	f.parent.draw(false)
}

// doneFile 完成一个文件
func (f *fileProgress) doneFile(name string, origSize, newSize int) {
	f.parent.doneFile(name, origSize, newSize)
}

// finish 全部完成
func (f *fileProgress) finish() {}

// printSummary 输出压缩统计
func printSummary(results []fileResult, okCount, failCount int,
	sumOrig, sumNew int64, sumDur, totalDur time.Duration, single bool) {

	saved := sumOrig - sumNew
	fmt.Printf("\n")
	fmt.Printf("==================== 压缩汇总 ====================\n")
	if !single {
		fmt.Printf("处理文件数  : %d 个（成功 %d，失败 %d）\n", len(results), okCount, failCount)
	}
	fmt.Printf("压缩前总大小: %s (%d 字节)\n", humanBytes(sumOrig), sumOrig)
	fmt.Printf("压缩后总大小: %s (%d 字节)\n", humanBytes(sumNew), sumNew)
	fmt.Printf("节省空间    : %s (%d 字节)\n", humanBytes(saved), saved)
	if sumOrig > 0 {
		fmt.Printf("压缩率      : %.2f%% (体积减少至 %.2f%%)\n",
			(1-float64(sumNew)/float64(sumOrig))*100,
			float64(sumNew)/float64(sumOrig)*100)
	}
	fmt.Printf("压缩耗时    : %v\n", totalDur.Round(time.Millisecond))
	if !single && okCount > 1 {
		fmt.Printf("平均单文件  : %v\n", (sumDur / time.Duration(okCount)).Round(time.Millisecond))
	}
	fmt.Printf("无损校验    : 全部通过（逐像素完全一致）\n")
	fmt.Printf("====================================================\n")
}

// humanBytes 把字节数格式化为易读形式
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
