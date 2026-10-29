package main

import (
	"compress/zlib"
	"io"

	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/foobaz/go-zopfli/zopfli"
	"hash/crc32"
	"image/color"
	"math"
	"sort"
)

// ---------------------------------------------------------------------------
// 规范化像素模型
// ---------------------------------------------------------------------------

// pixels 是解码后的规范化像素缓冲：每像素 4 个 uint16（直通道 alpha），
// 取值 0..65535，转换规则严格遵循 PNG 规范的位深缩放。
//
// 规范化是"可证明无损"的基础：任何目标色彩模型只要能用目标位深精确
// 还原出这些值，就一定是逐像素无损的。
type pixels struct {
	w, h int
	rgba []uint16 // 长度 w*h*4
}

// stepFor 返回该位深下一个采样单位对应的 16 位跨度。
// PNG 规范：位深 d 的样本 s 对应 8 位视图中的 s*255/(2^d-1)，
// 扩展到 16 位即 s*65535/(2^d-1)。
func stepFor(depth int) int {
	switch depth {
	case 1:
		return 65535
	case 2:
		return 21845
	case 4:
		return 4369
	case 8:
		return 257
	case 16:
		return 1
	}
	return 0
}

// expandSample 把位深 d 的采样值扩展为 16 位规范值
func expandSample(v uint16, depth int) uint16 {
	return uint16(int(v) * stepFor(depth))
}

// toCanonical 把解码后的源图像转换为规范化 16 位 RGBA 像素
func toCanonical(src *sourceImage) *pixels {
	p := &pixels{w: src.w, h: src.h, rgba: make([]uint16, src.w*src.h*4)}
	for i := 0; i < src.w*src.h; i++ {
		base := i * src.channels
		dst := i * 4
		switch src.colorType {
		case ctGray:
			g := expandSample(src.samples[base], src.bitDepth)
			p.rgba[dst], p.rgba[dst+1], p.rgba[dst+2] = g, g, g
			p.rgba[dst+3] = 65535
			if src.trnsGray != nil && src.samples[base] == *src.trnsGray {
				p.rgba[dst+3] = 0
			}
		case ctGrayAlpha:
			g := expandSample(src.samples[base], src.bitDepth)
			p.rgba[dst], p.rgba[dst+1], p.rgba[dst+2] = g, g, g
			p.rgba[dst+3] = expandSample(src.samples[base+1], src.bitDepth)
		case ctRGB:
			r := expandSample(src.samples[base], src.bitDepth)
			g := expandSample(src.samples[base+1], src.bitDepth)
			b := expandSample(src.samples[base+2], src.bitDepth)
			p.rgba[dst], p.rgba[dst+1], p.rgba[dst+2] = r, g, b
			p.rgba[dst+3] = 65535
			if src.trnsRGB != nil &&
				src.samples[base] == src.trnsRGB[0] &&
				src.samples[base+1] == src.trnsRGB[1] &&
				src.samples[base+2] == src.trnsRGB[2] {
				p.rgba[dst+3] = 0
			}
		case ctRGBA:
			p.rgba[dst] = expandSample(src.samples[base], src.bitDepth)
			p.rgba[dst+1] = expandSample(src.samples[base+1], src.bitDepth)
			p.rgba[dst+2] = expandSample(src.samples[base+2], src.bitDepth)
			p.rgba[dst+3] = expandSample(src.samples[base+3], src.bitDepth)
		case ctPalette:
			idx := int(src.samples[base])
			if idx < len(src.palette) {
				c := src.palette[idx]
				p.rgba[dst] = uint16(c.R) * 257
				p.rgba[dst+1] = uint16(c.G) * 257
				p.rgba[dst+2] = uint16(c.B) * 257
				p.rgba[dst+3] = uint16(c.A) * 257
			} else {
				p.rgba[dst+3] = 65535
			}
		}
	}
	return p
}

// ---------------------------------------------------------------------------
// 色彩模型决策
// ---------------------------------------------------------------------------

// modelKind 标识一种色彩模型
type modelKind int

const (
	modelRGBA      modelKind = iota // 真彩色 + Alpha
	modelRGB                        // 真彩色（丢弃不透明 alpha）
	modelGray                       // 灰度
	modelGrayAlpha                  // 灰度 + Alpha
	modelPalette                    // 索引色（调色板 + tRNS）
)

// model 是一种已确定的目标色彩模型。
// 每个模型都必须能通过 buildScanlines 精确还原原始像素。
type model struct {
	kind     modelKind
	bitDepth int
	label    string

	// 索引色相关
	palette  []color.NRGBA
	palAlpha []byte // tRNS：每项调色板条目的 alpha
	palIndex map[uint64]uint8

	// 灰度/真彩的单色透明（tRNS），存储原始样本值（非缩放值）
	trnsGray *uint16
	trnsRGB  *[3]uint16
}

// channels 返回模型的通道数
func (m *model) channels() int {
	switch m.kind {
	case modelGray:
		return 1
	case modelRGB:
		return 3
	case modelGrayAlpha:
		return 2
	case modelRGBA:
		return 4
	case modelPalette:
		return 1
	}
	return 0
}

// filterBPP 返回过滤算法使用的字节每像素数
func (m *model) filterBPP() int {
	b := m.channels() * m.bitDepth
	if b%8 != 0 {
		return 1
	}
	return b / 8
}

// colorType 返回 PNG 色彩类型编号
func (m *model) colorType() int {
	switch m.kind {
	case modelGray:
		return ctGray
	case modelRGB:
		return ctRGB
	case modelGrayAlpha:
		return ctGrayAlpha
	case modelRGBA:
		return ctRGBA
	case modelPalette:
		return ctPalette
	}
	return ctRGBA
}

// paletteKey 把规范化 RGBA 打包为去重键
func paletteKey(r, g, b, a uint16) uint64 {
	return uint64(r)<<48 | uint64(g)<<32 | uint64(b)<<16 | uint64(a)
}

// imageStats 是一次遍历得到的图像特征，用于色彩模型决策
type imageStats struct {
	allOpaque    bool
	allGray      bool
	allFits8     bool
	binaryAlpha  bool      // alpha 只取 0 或 65535
	singleTrans  bool      // 透明像素只有一个 RGB 取值
	transRGB     [3]uint16 // 该透明 RGB 取值
	transparentN int       // 透明像素数量
	opaqueUsesT  bool      // 存在不透明像素占用了透明色
	distinct     int       // 不同颜色数量（超过 256 即截断）
}

// analyze 扫描像素统计特征。
// 这是"直接选最优模型"的基础：避免为每个候选都做一次完整压缩评估。
func analyze(p *pixels) *imageStats {
	st := &imageStats{
		allOpaque:   true,
		allGray:     true,
		allFits8:    true,
		binaryAlpha: true,
		singleTrans: true,
	}
	seen := make(map[uint64]struct{}, 257)
	transSet := make(map[uint64]struct{}, 4)
	var transRGB [3]uint16
	haveTrans := false

	for i := 0; i < p.w*p.h; i++ {
		base := i * 4
		r, g, b, a := p.rgba[base], p.rgba[base+1], p.rgba[base+2], p.rgba[base+3]
		if a != 65535 {
			st.allOpaque = false
		}
		if a != 0 && a != 65535 {
			st.binaryAlpha = false
		}
		if r != g || g != b {
			st.allGray = false
		}
		if r%257 != 0 || g%257 != 0 || b%257 != 0 || a%257 != 0 {
			st.allFits8 = false
		}
		key := paletteKey(r, g, b, a)
		if _, ok := seen[key]; !ok {
			if len(seen) < 257 {
				seen[key] = struct{}{}
			}
		}
		if a == 0 {
			st.transparentN++
			transSet[uint64(r)<<16|uint64(g)<<8|uint64(b)] = struct{}{}
			if haveTrans && (transRGB[0] != r || transRGB[1] != g || transRGB[2] != b) {
				st.singleTrans = false
			}
			transRGB = [3]uint16{r, g, b}
			haveTrans = true
		}
	}
	st.distinct = len(seen)

	// 检查不透明像素是否占用了透明色
	if st.transparentN > 0 {
		for i := 0; i < p.w*p.h; i++ {
			base := i * 4
			r, g, b, a := p.rgba[base], p.rgba[base+1], p.rgba[base+2], p.rgba[base+3]
			if a != 65535 {
				continue
			}
			if haveTrans && transRGB[0] == r && transRGB[1] == g && transRGB[2] == b {
				st.opaqueUsesT = true
			}
			if st.allGray {
				// 灰度场景：不透明像素若等于透明灰阶同样不适用
				if _, ok := transSet[uint64(r)]; ok {
					st.opaqueUsesT = true
				}
			}
		}
	}
	st.transRGB = transRGB
	return st
}

// chooseModel 依据图像特征直接选出最优色彩模型。
//
// 决策依据（按体积收益从大到小）：
//  1. 颜色数 ≤ 256 → 索引色：每像素 1 字节（甚至 0.5/0.25 字节），收益最大
//  2. 全不透明 → 真彩色：省掉整个 alpha 通道
//  3. R=G=B → 灰度：省掉两个颜色通道
//  4. 其余 → RGBA：保持原始信息
//
// 所有降级都要求"可精确还原"，不满足则退回上一级。
func chooseModel(p *pixels, st *imageStats) *model {
	// --- 1. 索引色 ---
	if st.distinct <= 256 && st.allFits8 {
		if m := buildPaletteModel(p); m != nil {
			return m
		}
	}

	// --- 2. 灰度 / 灰度+Alpha ---
	if st.allGray {
		if st.allOpaque {
			// 选能容纳数据的最浅位深：体积最小
			for _, d := range []int{1, 2, 4, 8, 16} {
				if grayFitsDepth(p, d) {
					return &model{kind: modelGray, bitDepth: d, label: fmt.Sprintf("灰度/%dbit", d)}
				}
			}
		} else {
			// 单一透明灰阶 → 灰度+tRNS（比灰度+Alpha 省一个通道）
			if st.binaryAlpha && st.singleTrans && st.transparentN > 0 && !st.opaqueUsesT {
				for _, d := range []int{8, 16} {
					if grayFitsDepth(p, d) {
						tg := uint16(int(st.transRGB[0]) / stepFor(d))
						return &model{
							kind: modelGray, bitDepth: d,
							label:    fmt.Sprintf("灰度+tRNS/%dbit", d),
							trnsGray: &tg,
						}
					}
				}
			}
			// 灰度+Alpha
			for _, d := range []int{8, 16} {
				if grayAFitsDepth(p, d) {
					return &model{kind: modelGrayAlpha, bitDepth: d, label: fmt.Sprintf("灰度+Alpha/%dbit", d)}
				}
			}
		}
	} else if !st.allOpaque && st.binaryAlpha && st.singleTrans &&
		st.transparentN > 0 && !st.opaqueUsesT {
		// --- 3. 真彩色 + tRNS 单色透明 ---
		for _, d := range []int{8, 16} {
			if rgbFitsDepth(p, d) {
				step := stepFor(d)
				t := [3]uint16{
					uint16(int(st.transRGB[0]) / step),
					uint16(int(st.transRGB[1]) / step),
					uint16(int(st.transRGB[2]) / step),
				}
				return &model{
					kind: modelRGB, bitDepth: d,
					label:   fmt.Sprintf("真彩+tRNS/%dbit", d),
					trnsRGB: &t,
				}
			}
		}
	}

	// --- 4. 真彩色（丢弃不透明 alpha） ---
	if st.allOpaque {
		d := 16
		label := "真彩/16bit"
		if st.allFits8 {
			d = 8
			label = "真彩/8bit"
		}
		return &model{kind: modelRGB, bitDepth: d, label: label}
	}

	// --- 5. RGBA ---
	d := 16
	label := "RGBA/16bit"
	if st.allFits8 {
		d = 8
		label = "RGBA/8bit"
	}
	return &model{kind: modelRGBA, bitDepth: d, label: label}
}

// buildPaletteModel 构建索引色模型。调色板按颜色频率降序排列，
// 让高频颜色占据低位索引，提升相邻像素索引的相似度。
func buildPaletteModel(p *pixels) *model {
	counts := make(map[uint64]int, 256)
	for i := 0; i < p.w*p.h; i++ {
		base := i * 4
		counts[paletteKey(p.rgba[base], p.rgba[base+1], p.rgba[base+2], p.rgba[base+3])]++
	}
	if len(counts) > 256 {
		return nil
	}

	keys := make([]uint64, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	// 频率降序；频率相同时按颜色值升序，保证结果稳定可复现
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})

	m := &model{
		kind:     modelPalette,
		bitDepth: 8,
		label:    "索引色",
		palette:  make([]color.NRGBA, 0, len(keys)),
		palIndex: make(map[uint64]uint8, len(keys)),
	}
	for _, k := range keys {
		r := uint16(k >> 48)
		g := uint16(k >> 32)
		b := uint16(k >> 16)
		a := uint16(k)
		if r%257 != 0 || g%257 != 0 || b%257 != 0 || a%257 != 0 {
			return nil
		}
		m.palIndex[k] = uint8(len(m.palette))
		m.palette = append(m.palette, color.NRGBA{
			R: uint8(r / 257), G: uint8(g / 257), B: uint8(b / 257), A: uint8(a / 257),
		})
	}

	// 位深按调色板大小收缩
	switch {
	case len(m.palette) <= 2:
		m.bitDepth = 1
	case len(m.palette) <= 4:
		m.bitDepth = 2
	case len(m.palette) <= 16:
		m.bitDepth = 4
	}

	// tRNS：写到最后一个非不透明条目为止（规范允许 tRNS 比调色板短）
	lastNonOpaque := -1
	for i, e := range m.palette {
		if e.A != 255 {
			lastNonOpaque = i
		}
	}
	if lastNonOpaque >= 0 {
		m.palAlpha = make([]byte, lastNonOpaque+1)
		for i := 0; i <= lastNonOpaque; i++ {
			m.palAlpha[i] = m.palette[i].A
		}
	}
	return m
}

// grayFitsDepth 判断灰度值能否在给定位深下精确表示
func grayFitsDepth(p *pixels, depth int) bool {
	step := stepFor(depth)
	for i := 0; i < p.w*p.h; i++ {
		if int(p.rgba[i*4])%step != 0 {
			return false
		}
	}
	return true
}

// grayAFitsDepth 判断灰度+Alpha 能否在给定位深下精确表示
func grayAFitsDepth(p *pixels, depth int) bool {
	step := stepFor(depth)
	for i := 0; i < p.w*p.h; i++ {
		if int(p.rgba[i*4])%step != 0 || int(p.rgba[i*4+3])%step != 0 {
			return false
		}
	}
	return true
}

// rgbFitsDepth 判断真彩能否在给定位深下精确表示
func rgbFitsDepth(p *pixels, depth int) bool {
	step := stepFor(depth)
	for i := 0; i < p.w*p.h; i++ {
		for c := 0; c < 3; c++ {
			if int(p.rgba[i*4+c])%step != 0 {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 扫描线构建
// ---------------------------------------------------------------------------

// buildScanlines 按模型生成逐行原始（未过滤）数据
func buildScanlines(p *pixels, m *model) ([][]byte, error) {
	w, h := p.w, p.h
	ch := m.channels()
	depth := m.bitDepth
	step := stepFor(depth)
	rows := make([][]byte, h)
	rowBytes := (w*ch*depth + 7) / 8

	for y := 0; y < h; y++ {
		buf := make([]byte, rowBytes)
		switch m.kind {
		case modelPalette:
			for x := 0; x < w; x++ {
				base := (y*w + x) * 4
				idx, ok := m.palIndex[paletteKey(p.rgba[base], p.rgba[base+1], p.rgba[base+2], p.rgba[base+3])]
				if !ok {
					return nil, fmt.Errorf("调色板缺少颜色")
				}
				writeBits(buf, x, depth, uint32(idx))
			}
		case modelGray:
			for x := 0; x < w; x++ {
				g := int(p.rgba[(y*w+x)*4])
				writeBits(buf, x, depth, uint32(g/step))
			}
		case modelGrayAlpha:
			for x := 0; x < w; x++ {
				base := (y*w + x) * 4
				writeBits(buf, x*2, depth, uint32(int(p.rgba[base])/step))
				writeBits(buf, x*2+1, depth, uint32(int(p.rgba[base+3])/step))
			}
		case modelRGB:
			for x := 0; x < w; x++ {
				base := (y*w + x) * 4
				writeBits(buf, x*3, depth, uint32(int(p.rgba[base])/step))
				writeBits(buf, x*3+1, depth, uint32(int(p.rgba[base+1])/step))
				writeBits(buf, x*3+2, depth, uint32(int(p.rgba[base+2])/step))
			}
		case modelRGBA:
			for x := 0; x < w; x++ {
				base := (y*w + x) * 4
				writeBits(buf, x*4, depth, uint32(int(p.rgba[base])/step))
				writeBits(buf, x*4+1, depth, uint32(int(p.rgba[base+1])/step))
				writeBits(buf, x*4+2, depth, uint32(int(p.rgba[base+2])/step))
				writeBits(buf, x*4+3, depth, uint32(int(p.rgba[base+3])/step))
			}
		}
		rows[y] = buf
	}
	return rows, nil
}

// writeBits 以 PNG 规定的 MSB 优先方式写入第 index 个样本
func writeBits(buf []byte, index, depth int, val uint32) {
	switch depth {
	case 8:
		buf[index] = byte(val)
	case 16:
		buf[index*2] = byte(val >> 8)
		buf[index*2+1] = byte(val)
	case 1, 2, 4:
		bit := index * depth
		byteIdx := bit / 8
		shift := uint(8 - depth - bit%8)
		mask := byte((1 << depth) - 1)
		buf[byteIdx] = (buf[byteIdx] &^ (mask << shift)) | (byte(val&uint32(mask)) << shift)
	}
}

// ---------------------------------------------------------------------------
// PNG 过滤策略
// ---------------------------------------------------------------------------

// 五种标准过滤器
const (
	fNone = iota
	fSub
	fUp
	fAvg
	fPaeth
)

// filterLine 对一行数据施加指定过滤器
func filterLine(ft int, cur, prev []byte, bpp int) []byte {
	out := make([]byte, len(cur))
	for i := range cur {
		var a, b, c byte
		if i >= bpp {
			a = cur[i-bpp]
			if len(prev) > i-bpp {
				c = prev[i-bpp]
			}
		}
		if len(prev) > i {
			b = prev[i]
		}
		var x byte
		switch ft {
		case fNone:
			x = cur[i]
		case fSub:
			x = cur[i] - a
		case fUp:
			x = cur[i] - b
		case fAvg:
			x = cur[i] - byte((int(a)+int(b))/2)
		case fPaeth:
			x = cur[i] - paethPredictor(a, b, c)
		}
		out[i] = x
	}
	return out
}

// scoreMinSum 最小和启发式：把字节当作有符号数累加绝对值。
// 这是 libpng / PNG 规范推荐的经典启发式。
func scoreMinSum(line []byte) float64 {
	s := 0.0
	for _, v := range line {
		if v > 127 {
			s += float64(256 - int(v))
		} else {
			s += float64(v)
		}
	}
	return s
}

// scoreEntropy 香农熵启发式：熵越低越可压缩
func scoreEntropy(line []byte) float64 {
	var hist [256]int
	for _, v := range line {
		hist[v]++
	}
	total := float64(len(line))
	if total == 0 {
		return 0
	}
	e := 0.0
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		e -= p * math.Log2(p)
	}
	return e
}

// scoreBigram 相邻字节对多样性启发式：种类越少说明重复越多、越可压缩。
//
// 实测显示该策略与最小和/最小熵策略互补：某些图像上它明显更优，
// 因此三种逐行策略都需要保留。
func scoreBigram(line []byte) float64 {
	if len(line) < 2 {
		return 0
	}
	seen := make(map[uint16]struct{}, len(line))
	for i := 0; i+1 < len(line); i++ {
		seen[uint16(line[i])<<8|uint16(line[i+1])] = struct{}{}
	}
	return float64(len(seen))
}

// pickPerLine 为每一行选出最优过滤器（取 5 种过滤器中评分最低者）。
//
// 这是性能关键点：5 种过滤器的计算结果对所有评分函数是共享的，
// 因此一次算出 5 行再分别评分，比"每个评分函数各算一遍"快得多。
func pickPerLine(rows [][]byte, bpp int, scorer func([]byte) float64) []int {
	filters := make([]int, len(rows))
	prev := make([]byte, len(rows[0]))
	// 复用缓冲区以避免每行分配 5 个新切片
	variants := make([][]byte, 5)
	for i := range variants {
		variants[i] = make([]byte, len(rows[0]))
	}

	for i, row := range rows {
		best, bestScore := 0, math.Inf(1)
		for ft := 0; ft < 5; ft++ {
			variants[ft] = filterLineTo(variants[ft], row, prev, bpp, ft)
			s := scorer(variants[ft])
			if s < bestScore {
				bestScore, best = s, ft
			}
		}
		filters[i] = best
		prev = row
	}
	return filters
}

// pickPerLineMulti 用多个评分函数同时为每行选出最优过滤器。
//
// 与 pickPerLine 的区别：5 种过滤器的计算结果只做一次，
// 供所有评分函数复用。当需要多个逐行策略时，
// 用本函数可把过滤计算量降到 1/N。
//
// 返回值是每个评分函数对应的过滤器序列。
func pickPerLineMulti(rows [][]byte, bpp int, scorers []func([]byte) float64) [][]int {
	results := make([][]int, len(scorers))
	for i := range results {
		results[i] = make([]int, len(rows))
	}

	prev := make([]byte, len(rows[0]))
	variants := make([][]byte, 5)
	for i := range variants {
		variants[i] = make([]byte, len(rows[0]))
	}
	scores := make([]float64, len(scorers))
	bestScores := make([]float64, len(scorers))

	for i, row := range rows {
		// 5 种过滤器只算一次
		for ft := 0; ft < 5; ft++ {
			variants[ft] = filterLineTo(variants[ft], row, prev, bpp, ft)
		}
		// 各评分函数分别评分
		for si, sc := range scorers {
			best, bestScore := 0, math.Inf(1)
			for ft := 0; ft < 5; ft++ {
				s := sc(variants[ft])
				if s < bestScore {
					bestScore, best = s, ft
				}
			}
			results[si][i] = best
			scores[si] = bestScore
			bestScores[si] = bestScore
		}
		prev = row
	}
	_ = scores
	_ = bestScores
	return results
}

// filterLineTo 就地计算过滤结果，复用目标缓冲区以减少分配
func filterLineTo(dst, cur, prev []byte, bpp, ft int) []byte {
	if cap(dst) < len(cur) {
		dst = make([]byte, len(cur))
	}
	dst = dst[:len(cur)]
	for i := range cur {
		var a, b, c byte
		if i >= bpp {
			a = cur[i-bpp]
			if len(prev) > i-bpp {
				c = prev[i-bpp]
			}
		}
		if len(prev) > i {
			b = prev[i]
		}
		var x byte
		switch ft {
		case fNone:
			x = cur[i]
		case fSub:
			x = cur[i] - a
		case fUp:
			x = cur[i] - b
		case fAvg:
			x = cur[i] - byte((int(a)+int(b))/2)
		case fPaeth:
			x = cur[i] - paethPredictor(a, b, c)
		}
		dst[i] = x
	}
	return dst
}

// buildFiltered 依据逐行过滤器选择构建完整的数据流（含过滤器类型字节）
func buildFiltered(rows [][]byte, filters []int, bpp int) []byte {
	var out bytes.Buffer
	prev := make([]byte, len(rows[0]))
	for i, row := range rows {
		out.WriteByte(byte(filters[i]))
		out.Write(filterLine(filters[i], row, prev, bpp))
		prev = row
	}
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// PNG 文件组装
// ---------------------------------------------------------------------------

// extras 分为前置块（必须在 PLTE 之前）与后置块（PLTE 之后、IDAT 之前）
type extras struct {
	pre  []pngChunk
	post []pngChunk
}

// writeChunk 写入一个带 CRC32 的 PNG 数据块
func writeChunk(buf *bytes.Buffer, typ string, data []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(data)))
	buf.Write(l[:])
	crc := crc32.NewIEEE()
	buf.WriteString(typ)
	buf.Write(data)
	crc.Write([]byte(typ))
	crc.Write(data)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc.Sum32())
	buf.Write(c[:])
}

// buildPNG 把过滤后的数据流与色彩模型组装为完整 PNG 文件
func buildPNG(p *pixels, m *model, filtered []byte, ex extras) []byte {
	var buf bytes.Buffer
	buf.WriteString(pngSignature)

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(p.w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(p.h))
	ihdr[8] = byte(m.bitDepth)
	ihdr[9] = byte(m.colorType())
	ihdr[10] = 0 // 压缩方法
	ihdr[11] = 0 // 过滤方法
	ihdr[12] = 0 // 非隔行
	writeChunk(&buf, "IHDR", ihdr)

	// 前置辅助块：cHRM / gAMA / iCCP / sRGB / sBIT 必须排在 PLTE 之前
	for _, c := range ex.pre {
		writeChunk(&buf, c.typ, c.data)
	}

	if m.kind == modelPalette {
		plte := make([]byte, 0, len(m.palette)*3)
		for _, e := range m.palette {
			plte = append(plte, e.R, e.G, e.B)
		}
		writeChunk(&buf, "PLTE", plte)
		if len(m.palAlpha) > 0 {
			writeChunk(&buf, "tRNS", m.palAlpha)
		}
	} else if m.trnsGray != nil {
		t := make([]byte, 2)
		binary.BigEndian.PutUint16(t, *m.trnsGray)
		writeChunk(&buf, "tRNS", t)
	} else if m.trnsRGB != nil {
		t := make([]byte, 6)
		binary.BigEndian.PutUint16(t[0:], m.trnsRGB[0])
		binary.BigEndian.PutUint16(t[2:], m.trnsRGB[1])
		binary.BigEndian.PutUint16(t[4:], m.trnsRGB[2])
		writeChunk(&buf, "tRNS", t)
	}

	for _, c := range ex.post {
		writeChunk(&buf, c.typ, c.data)
	}

	writeChunk(&buf, "IDAT", filtered)
	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

// zopfliWithIters 用指定迭代次数做 Zopfli 压缩（仅供测试对比）
func zopfliWithIters(data []byte, iters int) []byte {
	var buf bytes.Buffer
	opts := &zopfli.Options{NumIterations: iters, BlockSplitting: true, BlockSplittingMax: 15, BlockType: 2}
	zopfli.ZlibCompress(opts, data, &buf)
	return buf.Bytes()
}

// compressFast2 供测试用
func compressFast2(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlibNew(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func zlibNew(b *bytes.Buffer) (io.WriteCloser, error) {
	return zlib.NewWriterLevel(b, zlib.BestCompression)
}
