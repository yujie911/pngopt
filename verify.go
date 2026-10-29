package main

import (
	"bytes"
	"fmt"
	stdpng "image/png"
	"os"
)

// readFile 读取文件
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// writeFile 写入文件
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// verifyLossless 解码输出文件并与原始像素逐点比对。
//
// 关键点：必须同时用 Go 标准库 image/png 独立解码一次。
// 自研解码器与自研编码器共享同一套约定，若 tRNS 等辅助块语义写错，
// 自校验会"自己验证自己"而漏过问题；标准库解码是独立实现，能发现这类缺陷。
func verifyLossless(orig *pixels, outBytes []byte, origRaw []byte) error {
	// 1) 自研解码器校验
	dec, err := decodePNG(outBytes)
	if err != nil {
		return fmt.Errorf("输出文件无法解码: %w", err)
	}
	if dec.w != orig.w || dec.h != orig.h {
		return fmt.Errorf("尺寸不一致: %dx%d vs %dx%d", dec.w, dec.h, orig.w, orig.h)
	}
	got := toCanonical(dec)
	for i := range orig.rgba {
		if orig.rgba[i] != got.rgba[i] {
			px := i / 4
			return fmt.Errorf("像素 %d,%d 的通道 %d 不一致: %d != %d",
				px%orig.w, px/orig.w, i%4, orig.rgba[i], got.rgba[i])
		}
	}

	// 2) Go 标准库交叉校验（独立实现路径）
	if err := verifyWithStdlib(origRaw, outBytes); err != nil {
		return fmt.Errorf("标准库交叉校验失败: %w", err)
	}
	return nil
}

// verifyWithStdlib 用 image/png 标准库解析并逐像素比对
func verifyWithStdlib(origRaw, outRaw []byte) error {
	oi, err := stdpng.Decode(bytes.NewReader(origRaw))
	if err != nil {
		return fmt.Errorf("原图标准库解码失败: %w", err)
	}
	ni, err := stdpng.Decode(bytes.NewReader(outRaw))
	if err != nil {
		return fmt.Errorf("输出图标准库解码失败: %w", err)
	}
	ob, nb := oi.Bounds(), ni.Bounds()
	if ob.Dx() != nb.Dx() || ob.Dy() != nb.Dy() {
		return fmt.Errorf("标准库读取到的尺寸不一致: %v vs %v", ob, nb)
	}
	for y := ob.Min.Y; y < ob.Max.Y; y++ {
		for x := ob.Min.X; x < ob.Max.X; x++ {
			or, og, ob2, oa := oi.At(x, y).RGBA()
			nr, ng, nb2, na := ni.At(x, y).RGBA()
			if or != nr || og != ng || ob2 != nb2 || oa != na {
				return fmt.Errorf("标准库视角下像素 (%d,%d) 不一致: 原=(%d,%d,%d,%d) 新=(%d,%d,%d,%d)",
					x, y, or, og, ob2, oa, nr, ng, nb2, na)
			}
		}
	}
	return nil
}
