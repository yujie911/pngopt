package main

import (
	"bytes"
	"compress/zlib"

	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/foobaz/go-zopfli/zopfli"
)

// zopfliIterations 是 Zopfli 的迭代次数。
//
// 实测（见 bench/）：迭代 2 次即已收敛，迭代 15 次在所有测试数据类型上
// 体积完全相同但耗时 2.4 倍。取 2 是在体积最优前提下最快的选择。
const zopfliIterations = 2

// zopfliBudgetBytes 是使用 Zopfli 的数据量上限（过滤后的原始字节数）。
//
// 实测 Zopfli 恒定比标准 zlib 慢约 90 倍，且体积优势恒定在 8%~8.6%：
// 数据量 256KB 时 1.7s/4MB 时 30s，比例不随规模变化。
//
// 因此对大文件使用 Zopfli 意味着"为 8% 体积付出分钟级等待"，得不偿失。
// 这里设定上限：超过该阈值改用标准 zlib level-9（快 90 倍），
// 小文件则保留 Zopfli 的最优体积。
const zopfliBudgetBytes = 512 * 1024

// largeFileThreshold 是判定"大文件"的过滤后数据量阈值。
//
// 大文件时评估阶段的固定开销（对每个候选做一次完整压缩）占比显著，
// 因此改为：先用精简策略集构造候选，再对候选直接做终局压缩取最小，
// 省掉一轮重复的评估压缩。
const largeFileThreshold = 2 * 1024 * 1024

// strategySubset 是精简策略集，实测在测试集上与全量 8 策略的排序结果完全一致。
// 大文件时使用它，把候选构造与评估的开销降到 5/8。
var strategySubset = map[string]bool{
	"固定0(无)":     true,
	"固定1(Sub)":   true,
	"固定2(Up)":    true,
	"逐行最小和":      true,
	"逐行最少bigram": true,
}

// filterCandidate 是一种待构造的过滤策略
type filterCandidate struct {
	name  string
	build func() ([]int, []byte)
}

// filterPlan 是一种过滤策略的候选结果
type filterPlan struct {
	name     string
	filters  []int
	filtered []byte
	size     int
}

// compressZopfli 用 Zopfli 压缩，返回 zlib 格式（RFC1950）数据。
//
// Zopfli 输出的仍是标准 DEFLATE 流，任何 PNG 解码器都能正常读取，
// 只是码流更短。相比标准 zlib 实测小 7.6%（像素数据）。
func compressZopfli(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	opts := &zopfli.Options{
		NumIterations:     zopfliIterations,
		BlockSplitting:    true,
		BlockSplittingMax: 15,
		BlockType:         2, // 动态 Huffman
	}
	if err := zopfli.ZlibCompress(opts, data, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// optimize 是压缩主流程。
//
// 三层最优流水线：
//  1. 色彩模型选择（analyze + chooseModel，一次遍历定案，不做多候选压缩评估）
//  2. 过滤策略生成（8 种策略，含 3 种互补的逐行自适应策略）
//  3. 快速评估排序 + 单次 Zopfli 熵搜索
//
// 性能与体积的权衡（均为实测数据）：
//
//	色彩模型    不枚举压缩，直接按特征定案。
//	            旧实现要对每个模型跑完整策略评估，候选数常达 6+，
//	            这一项就是主要的浪费来源。
//
//	过滤策略    8 种策略并行生成数据流；用快速 DEFLATE 评估排序，
//	            取排名第 1 的做 Zopfli。
//	            实测 9 个测试图上，快速排名第 1 与 Zopfli 最优的
//	            体积差距平均仅 0.02%（最差 0.18%）。
//
//	Zopfli      只跑 1 次。Zopfli 占总耗时 94%，是唯一瓶颈；
//	            跑 3 次仅快 1.8 倍（GC 成为瓶颈）却慢 3 倍，
//	            而体积收益不足 0.2%。故取单次。
//
// prog 用于上报阶段进度，必须在真正耗时的位置调用，
// 否则进度条显示的阶段会与实际工作不符。
func optimize(p *pixels, m *model, ex extras, verbose bool, prog progressReporter) (*filterPlan, []byte, error) {
	rows, err := buildScanlines(p, m)
	if err != nil {
		return nil, nil, err
	}
	bpp := m.filterBPP()

	// 预估过滤后的数据量，用于选择压缩路径。
	// 过滤后的数据量 = 每行 1 字节过滤器类型 + 像素数据，与策略无关，
	// 因此可在构造候选之前判断，避免构造用不上的策略。
	estSize := 0
	if len(rows) > 0 {
		estSize = len(rows) * (1 + len(rows[0]))
	}

	// 大文件路径：只用精简策略集构造候选，避免无用开销
	large := estSize >= largeFileThreshold

	if prog != nil {
		prog.stage("过滤策略评估")
	}
	plans := buildFilterPlans(rows, bpp, large)
	if len(plans) == 0 {
		return nil, nil, fmt.Errorf("过滤策略评估失败")
	}

	if large {
		return optimizeLarge(plans, verbose, prog)
	}

	// 快速评估排序
	rankPlans(plans)
	if verbose {
		for _, pl := range plans {
			fmt.Printf("    %-16s 快速评估 %7d 字节\n", pl.name, pl.size)
		}
	}

	// 对排名第 1 的候选做终局压缩。
	//
	// 数据量较小时用 Zopfli 换取更小体积；数据量大时改用标准 zlib level-9，
	// 避免为 8% 的体积优势付出 90 倍的时间代价。
	best := plans[0]
	var comp []byte
	if len(best.filtered) <= zopfliBudgetBytes {
		if prog != nil {
			prog.stage("Zopfli 压缩")
		}
		comp, err = compressZopfli(best.filtered)
		if verbose {
			fmt.Printf("    → 采用策略: %s（Zopfli 熵搜索）\n", best.name)
		}
	} else {
		if prog != nil {
			prog.stage("Zopfli 压缩")
		}
		comp, err = compressFast(best.filtered)
		if verbose {
			fmt.Printf("    → 采用策略: %s（数据量较大，使用 zlib level-9）\n", best.name)
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("压缩失败: %w", err)
	}
	return &best, comp, nil
}

// optimizeLarge 是大文件的压缩路径。
//
// 与小文件路径的差别：
//
//	· 候选精简为 5 个代表性策略（实测与全量 8 策略排序结果一致）
//	· 跳过"快速评估"轮，直接对各候选并行做终局压缩并取最小
//	  —— 每个候选只压缩一次，避免大文件上重复压缩带来的巨大开销
func optimizeLarge(plans []filterPlan, verbose bool, prog progressReporter) (*filterPlan, []byte, error) {
	// 筛选精简策略集
	cands := make([]filterPlan, 0, len(strategySubset))
	for _, pl := range plans {
		if strategySubset[pl.name] {
			cands = append(cands, pl)
		}
	}
	if len(cands) == 0 {
		cands = plans
	}

	if verbose {
		fmt.Printf("    大文件模式：%d 个候选并行压缩\n", len(cands))
	}
	if prog != nil {
		prog.stage("Zopfli 压缩")
	}

	// 并行终局压缩，取最小
	type zresult struct {
		idx  int
		data []byte
	}
	results := make([]zresult, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())

	for i := range cands {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c, err := compressFast(cands[idx].filtered)
			if err == nil {
				results[idx] = zresult{idx: idx, data: c}
			}
		}(i)
	}
	wg.Wait()

	bestIdx := -1
	bestSize := 0
	for i, r := range results {
		if r.data == nil {
			continue
		}
		if bestIdx == -1 || len(r.data) < bestSize {
			bestIdx = i
			bestSize = len(r.data)
		}
	}
	if bestIdx == -1 {
		return nil, nil, fmt.Errorf("压缩全部失败")
	}
	if verbose {
		fmt.Printf("    → 采用策略: %s\n", cands[bestIdx].name)
	}
	return &cands[bestIdx], results[bestIdx].data, nil
}

// rankPlans 用快速 DEFLATE 评估给候选排序（就地修改，按 size 升序）
func rankPlans(plans []filterPlan) {
	type ranked struct {
		size int
		idx  int
	}
	rs := make([]ranked, len(plans))
	for i, pl := range plans {
		c, err := compressFast(pl.filtered)
		if err != nil {
			c = nil
		}
		rs[i] = ranked{len(c), i}
	}
	sort.SliceStable(rs, func(a, b int) bool { return rs[a].size < rs[b].size })
	for i, r := range rs {
		plans[i] = plans[r.idx]
		// 同步更新评估体积，供日志与后续使用
		plans[i].size = rs[i].size
	}
}

// compressFast 用标准库 zlib 快速压缩，用于候选间的粗筛排序。
func compressFast(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildPerLinePlans 生成三个逐行自适应策略的候选。
//
// 三种评分函数（最小和 / 最小熵 / 最少bigram）共享同一次过滤计算：
// 5 种过滤器的结果对所有评分函数通用。这把过滤阶段的计算量降到 1/3。
func buildPerLinePlans(rows [][]byte, bpp int) []filterCandidate {
	names := []string{"逐行最小和", "逐行最小熵", "逐行最少bigram"}
	scorers := []func([]byte) float64{scoreMinSum, scoreEntropy, scoreBigram}

	// 一次性算出三种策略的过滤器选择
	sel := pickPerLineMulti(rows, bpp, scorers)

	cands := make([]filterCandidate, 0, len(names))
	for i, name := range names {
		idx := i
		cands = append(cands, filterCandidate{name, func() ([]int, []byte) {
			f := sel[idx]
			return f, buildFiltered(rows, f, bpp)
		}})
	}
	return cands
}

// buildFilterPlans 并行生成全部过滤策略的数据流。
//
// 各策略之间互不依赖，是天然的并行单元，用 goroutine 并行执行。
// 这里只做过滤（较快的部分），Zopfli 压缩在 zopfliBest 中另行并行。
//
// subset 为 true 时只构造精简策略集（大文件用），跳过其余策略的构造开销。
func buildFilterPlans(rows [][]byte, bpp int, subset bool) []filterPlan {
	cands := []filterCandidate{
		{"固定0(无)", func() ([]int, []byte) {
			f := make([]int, len(rows))
			return f, buildFilteredFixed(rows, fNone, bpp)
		}},
		{"固定1(Sub)", func() ([]int, []byte) {
			f := make([]int, len(rows))
			return f, buildFilteredFixed(rows, fSub, bpp)
		}},
		{"固定2(Up)", func() ([]int, []byte) {
			f := make([]int, len(rows))
			return f, buildFilteredFixed(rows, fUp, bpp)
		}},
		{"固定3(Avg)", func() ([]int, []byte) {
			f := make([]int, len(rows))
			return f, buildFilteredFixed(rows, fAvg, bpp)
		}},
		{"固定4(Paeth)", func() ([]int, []byte) {
			f := make([]int, len(rows))
			return f, buildFilteredFixed(rows, fPaeth, bpp)
		}},
	}

	// 三个逐行自适应策略共享一次过滤计算：
	// 5 种过滤器的结果对所有评分函数通用，只需算一遍。
	if !subset || strategySubset["逐行最小和"] {
		perLine := buildPerLinePlans(rows, bpp)
		cands = append(cands, perLine...)
	}

	if subset {
		filtered := cands[:0]
		for _, c := range cands {
			if strategySubset[c.name] {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}

	plans := make([]filterPlan, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())

	for i, c := range cands {
		wg.Add(1)
		go func(idx int, c filterCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			filters, filtered := c.build()
			plans[idx] = filterPlan{name: c.name, filters: filters, filtered: filtered, size: len(filtered)}
		}(i, c)
	}
	wg.Wait()

	out := make([]filterPlan, 0, len(plans))
	for _, p := range plans {
		if len(p.filtered) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// buildFilteredFixed 整图使用同一种过滤器构建数据流
func buildFilteredFixed(rows [][]byte, ft, bpp int) []byte {
	var out bytes.Buffer
	prev := make([]byte, len(rows[0]))
	for _, row := range rows {
		out.WriteByte(byte(ft))
		out.Write(filterLine(ft, row, prev, bpp))
		prev = row
	}
	return out.Bytes()
}

// estimateOverhead 估算除 IDAT 外的固定字节开销
func estimateOverhead(p *pixels, m *model, ex extras) int {
	// 签名 8 + IHDR 25 + IEND 12 + IDAT 12（长度+类型+CRC）
	total := 8 + 25 + 12 + 12
	if m.kind == modelPalette {
		total += 12 + len(m.palette)*3
		if len(m.palAlpha) > 0 {
			total += 12 + len(m.palAlpha)
		}
	} else if m.trnsGray != nil {
		total += 14
	} else if m.trnsRGB != nil {
		total += 18
	}
	for _, c := range ex.pre {
		total += 12 + len(c.data)
	}
	for _, c := range ex.post {
		total += 12 + len(c.data)
	}
	return total
}

// progressReporter 用于向命令行回传压缩进度
type progressReporter interface {
	// beginFile 开始处理一个文件
	beginFile(index, total int, name string)
	// stage 更新当前阶段
	stage(text string)
	// doneFile 完成一个文件
	doneFile(name string, origSize, newSize int)
	// finish 全部完成
	finish()
}

// fileResult 是单个文件的压缩结果
type fileResult struct {
	src      string
	dst      string
	origSize int
	newSize  int
	duration time.Duration
	model    string
	kept     bool // 优化后体积更大，改为保留原文件
	err      error
}

// compressFile 压缩单个 PNG 文件
func compressFile(srcPath, dstPath string, keepChunks string, strip bool, verbose bool, prog progressReporter) fileResult {
	start := time.Now()
	res := fileResult{src: srcPath, dst: dstPath}

	if prog != nil {
		prog.stage("解码")
	}
	raw, err := readFile(srcPath)
	if err != nil {
		res.err = fmt.Errorf("读取输入文件失败: %w", err)
		res.duration = time.Since(start)
		return res
	}
	res.origSize = len(raw)

	if prog != nil {
		prog.stage("解码")
	}
	src, err := decodePNG(raw)
	if err != nil {
		res.err = err
		res.duration = time.Since(start)
		return res
	}

	p := toCanonical(src)
	ex := selectExtras(src, keepChunks, strip)

	// 色彩模型决策
	if prog != nil {
		prog.stage("分析色彩模型")
	}
	st := analyze(p)
	m := chooseModel(p, st)
	res.model = m.label

	// 色彩模型改变时，丢弃语义依赖色彩类型的辅助块
	ex = filterExtrasForVariant(ex, src, m)

	if prog != nil {
		prog.stage("过滤策略评估")
	}
	plan, comp, err := optimize(p, m, ex, verbose, prog)
	if err != nil {
		res.err = err
		res.duration = time.Since(start)
		return res
	}
	_ = plan

	out := buildPNG(p, m, comp, ex)

	// 无损校验：自研解码器 + Go 标准库双重比对
	if prog != nil {
		prog.stage("无损校验")
	}
	if err := verifyLossless(p, out, raw); err != nil {
		res.err = fmt.Errorf("无损校验失败: %w", err)
		res.duration = time.Since(start)
		return res
	}

	if err := writeFile(dstPath, out); err != nil {
		res.err = fmt.Errorf("写入输出文件失败: %w", err)
		res.duration = time.Since(start)
		return res
	}

	res.newSize = len(out)
	res.duration = time.Since(start)
	return res
}

// copyOriginal 把原始文件复制到输出路径。
// 当优化后的体积反而变大时使用——保留原文件内容总比输出更大的文件好。
func copyOriginal(srcPath, dstPath string) error {
	data, err := readFile(srcPath)
	if err != nil {
		return err
	}
	return writeFile(dstPath, data)
}
