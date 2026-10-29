package main

import (
	"strings"
)

// defaultKeepChunks 是默认保留的辅助数据块。
//
// 这些块影响图片的显示效果（色彩空间、伽马、背景色等），
// 丢弃会导致渲染结果变化，因此默认保留。
// 纯元数据块（tEXt/zTXt/iTXt）体积可能很大，默认丢弃以获得更小的输出。
var defaultKeepChunks = []string{"iccp", "gama", "srgb", "chrm", "sbit"}

// selectExtras 依据参数挑选需要保留的辅助数据块
func selectExtras(src *sourceImage, keepList string, strip bool) extras {
	var ex extras
	if strip {
		return ex
	}

	keep := map[string]bool{}
	if keepList != "" {
		for _, k := range strings.Split(keepList, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				keep[strings.ToLower(k)] = true
			}
		}
	} else {
		for _, k := range defaultKeepChunks {
			keep[k] = true
		}
	}

	// 必须排在 PLTE 之前的块
	preOrder := []string{"cHRM", "gAMA", "iCCP", "sRGB", "sBIT"}
	postOrder := []string{"bKGD", "hIST", "pHYs", "tIME", "tEXt", "zTXt", "iTXt"}

	for _, typ := range preOrder {
		if data, ok := src.extras[typ]; ok && keep[strings.ToLower(typ)] {
			ex.pre = append(ex.pre, pngChunk{typ: typ, data: data})
		}
	}
	for _, typ := range postOrder {
		if data, ok := src.extras[typ]; ok && keep[strings.ToLower(typ)] {
			ex.post = append(ex.post, pngChunk{typ: typ, data: data})
		}
	}
	return ex
}

// filterExtrasForVariant 剔除与目标色彩模型不兼容的辅助块。
//
// PNG 规范中部分辅助块的语义依赖色彩类型，色彩模型改变后必须一并丢弃，
// 否则会生成结构非法的文件：
//   - bKGD：灰度为 2 字节、真彩为 6 字节、索引色为 1 字节（调色板索引）
//   - sBIT：各通道有效位数随色彩类型与位深变化
//   - hIST：仅对索引色有意义，且与调色板顺序绑定
func filterExtrasForVariant(exIn extras, src *sourceImage, m *model) extras {
	out := exIn
	changed := src.colorType != m.colorType() || src.bitDepth != m.bitDepth
	if !changed {
		return out
	}

	drop := map[string]bool{"bKGD": true, "sBIT": true, "hIST": true}

	filter := func(list []pngChunk) []pngChunk {
		res := make([]pngChunk, 0, len(list))
		for _, c := range list {
			if drop[c.typ] {
				continue
			}
			res = append(res, c)
		}
		return res
	}
	out.pre = filter(out.pre)
	out.post = filter(out.post)
	return out
}
