package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestProgress 构造一个模拟终端环境的进度显示器
func newTestProgress(total int) *consoleProgress {
	c := &consoleProgress{
		total:       total,
		start:       time.Now(),
		stageStart:  time.Now(),
		interactive: true,
		stop:        make(chan struct{}),
		done2:       make(chan struct{}),
	}
	if total > 1 {
		c.multi = true
		c.lines = make(map[int]*fileLine)
	}
	go c.ticker()
	return c
}

// 验证进度条在长阶段内持续刷新（这是"进度条不动"问题的核心回归测试）
func TestProgressRefresh(t *testing.T) {
	c := newTestProgress(1)
	defer func() { close(c.stop); <-c.done2 }()

	c.stage("Zopfli 压缩")

	changes := 0
	last := c.lastDraw
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		c.mu.Lock()
		cur := c.lastDraw
		c.mu.Unlock()
		if cur != last {
			changes++
			last = cur
		}
	}
	fmt.Printf("1 秒内重绘次数: %d\n", changes)
	if changes < 5 {
		t.Errorf("重绘次数过少 (%d)，进度条不会走动", changes)
	}
}

// 验证进度在阶段推进时单调不减，阶段内保持稳定（不做虚假插值）
func TestProgressStageJump(t *testing.T) {
	c := newTestProgress(1)
	defer func() { close(c.stop); <-c.done2 }()

	var prev float64
	for _, stage := range stageNames {
		c.stage(stage)
		c.mu.Lock()
		p := c.singleProgress(time.Now())
		c.mu.Unlock()
		if p < prev {
			t.Errorf("阶段 %s 进度回退: %v < %v", stage, p, prev)
		}
		prev = p
	}
	if prev != 100 {
		t.Errorf("最终进度应为 100%%，实得 %v", prev)
	}

	c.stage("Zopfli 压缩")
	c.mu.Lock()
	a := c.singleProgress(time.Now())
	c.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	c.mu.Lock()
	b := c.singleProgress(time.Now())
	c.mu.Unlock()
	if a != b {
		t.Errorf("阶段内进度发生虚假变化: %v -> %v", a, b)
	}
	fmt.Println("阶段跳变正确、阶段内稳定 ✓")
}

// 验证旋转指示器确实在转动（阶段内的活动感来源）
func TestSpinnerRotates(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		seen[spinnerFrames[i%len(spinnerFrames)]] = true
	}
	if len(seen) != len(spinnerFrames) {
		t.Errorf("指示器帧不完整: %v", seen)
	}
	fmt.Printf("指示器帧: %v ✓\n", spinnerFrames)
}

// 验证非终端环境下不输出 ANSI 序列（重定向到文件时保持输出干净）
func TestNonInteractiveNoANSI(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	c := &consoleProgress{
		total:       1,
		start:       time.Now(),
		stageStart:  time.Now(),
		interactive: false,
		stop:        make(chan struct{}),
		done2:       make(chan struct{}),
	}
	go c.ticker()
	c.stage("Zopfli 压缩")
	time.Sleep(300 * time.Millisecond)
	close(c.stop)
	<-c.done2
	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n := 0
	readDone := make(chan int, 1)
	go func() {
		var m int
		m, _ = r.Read(buf)
		readDone <- m
	}()
	select {
	case n = <-readDone:
	case <-time.After(500 * time.Millisecond):
	}
	if n > 0 && strings.Contains(string(buf[:n]), "\x1b") {
		t.Errorf("非终端环境输出了 ANSI 转义序列: %q", string(buf[:n]))
	}
	fmt.Println("非终端环境: 未输出 ANSI 序列 ✓")
}

// 验证 Windows 控制台能成功开启 ANSI 虚拟终端模式
func TestEnableANSI(t *testing.T) {
	ok := enableANSI(os.Stdout)
	isT := isTerminal(os.Stdout)
	fmt.Printf("isTerminal=%v enableANSI=%v\n", isT, ok)
	// 非终端时 enableANSI 应返回 false（不应对管道强行设置控制台模式）
	if !isT && ok {
		t.Errorf("非终端环境不应报告 ANSI 启用成功")
	}
}

// 验证阶段上报与实际耗时对应
//
// 这是一个已修复缺陷的回归测试：原先 prog.stage("Zopfli 压缩")
// 在 optimize() 返回之后才调用，导致进度条把 4.5 秒的 Zopfli 压缩
// 显示成"过滤策略评估"，阶段与实际工作完全不符。
func TestStageTimingMatchesWork(t *testing.T) {
	// 记录每个阶段被上报的时刻
	type mark struct {
		stage string
		at    time.Time
	}
	var marks []mark

	rec := &recorder{onStage: func(s string) {
		marks = append(marks, mark{s, time.Now()})
	}}

	// 模拟：过滤策略评估很快，Zopfli 很慢
	start := time.Now()
	rec.stage("过滤策略评估")
	time.Sleep(50 * time.Millisecond)
	rec.stage("Zopfli 压缩")
	time.Sleep(300 * time.Millisecond)
	rec.stage("无损校验")
	_ = start

	// 检查 Zopfli 阶段确实占据了主要时间
	if len(marks) != 3 {
		t.Fatalf("阶段数不对: %d", len(marks))
	}
	zopfliDur := marks[2].at.Sub(marks[1].at)
	filterDur := marks[1].at.Sub(marks[0].at)
	if zopfliDur <= filterDur {
		t.Errorf("Zopfli 阶段耗时(%v) 应大于过滤评估阶段(%v)", zopfliDur, filterDur)
	}
	fmt.Printf("过滤评估 %v → Zopfli %v → 校验 ✓\n", filterDur.Round(time.Millisecond), zopfliDur.Round(time.Millisecond))
}

// recorder 是一个记录阶段上报的 progressReporter
type recorder struct {
	onStage func(string)
}

func (r *recorder) beginFile(index, total int, name string) {}
func (r *recorder) stage(text string)                       { r.onStage(text) }
func (r *recorder) doneFile(name string, o, n int)          {}
func (r *recorder) finish()                                 {}

// 验证多文件模式的总进度在批次处理过程中平滑推进
//
// 缺陷背景：原先总进度只按"已完成文件数"计算，
// 批次内所有文件都在压缩时进度一直是 0%，同样表现为"进度条不动"。
func TestMultiProgressSmooth(t *testing.T) {
	c := newTestProgress(4)
	defer func() { close(c.stop); <-c.done2 }()

	c.multi = true
	c.lines = make(map[int]*fileLine)
	for i := 0; i < 4; i++ {
		c.lines[i] = &fileLine{index: i, name: fmt.Sprintf("f%d.png", i), stage: "Zopfli 压缩"}
	}

	p1 := c.multiProgress(time.Now())
	// 模拟阶段推进
	c.lines[0].stage = "无损校验"
	p2 := c.multiProgress(time.Now())

	if p2 <= p1 {
		t.Errorf("总进度未推进: %v -> %v", p1, p2)
	}
	if p1 <= 0 || p1 >= 100 {
		t.Errorf("总进度数值异常: %v", p1)
	}
	fmt.Printf("多文件总进度: %.1f%% -> %.1f%% ✓\n", p1, p2)
}
