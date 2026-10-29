package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image/color"
)

// PNG 色彩类型常量
const (
	ctGray      = 0
	ctRGB       = 2
	ctPalette   = 3
	ctGrayAlpha = 4
	ctRGBA      = 6
)

const pngSignature = "\x89PNG\r\n\x1a\n"

// pngChunk 表示一个已解析的 PNG 数据块
type pngChunk struct {
	typ  string
	data []byte
}

// sourceImage 是解码后的规范化图像描述。
//
// samples 按像素存储，每像素 channels 个 uint16 采样值，取值范围是
// 源位深对应的原始值（未做任何缩放）。palette 色彩类型的 samples 存的是调色板索引。
type sourceImage struct {
	w, h      int
	colorType int
	bitDepth  int
	interlace int
	channels  int

	samples []uint16

	// 调色板（ctPalette），每项含 alpha
	palette []color.NRGBA
	// tRNS：灰度单色 / 真彩单色 / 调色板 alpha 数组
	trnsGray *uint16
	trnsRGB  *[3]uint16
	trnsPal  []byte

	// 需要透传的辅助数据块（iCCP/gAMA/sRGB/文本等）
	extras map[string][]byte
	order  []string
}

// channelsOf 返回指定色彩类型的通道数（调色板按 1 计）
func channelsOf(colorType int) int {
	switch colorType {
	case ctGray:
		return 1
	case ctRGB:
		return 3
	case ctPalette:
		return 1
	case ctGrayAlpha:
		return 2
	case ctRGBA:
		return 4
	}
	return 0
}

// validBitDepth 校验色彩类型与位深的合法组合
func validBitDepth(colorType, depth int) bool {
	switch colorType {
	case ctGray:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8 || depth == 16
	case ctRGB, ctRGBA:
		return depth == 8 || depth == 16
	case ctPalette:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8
	case ctGrayAlpha:
		return depth == 8 || depth == 16
	}
	return false
}

// decodePNG 解析并完整解码一个 PNG 文件
func decodePNG(data []byte) (*sourceImage, error) {
	if !bytes.HasPrefix(data, []byte(pngSignature)) {
		return nil, errors.New("不是有效的 PNG 文件（文件签名不匹配）")
	}

	img := &sourceImage{extras: map[string][]byte{}}
	var idat, plte, trns []byte
	seenIHDR := false

	pos := len(pngSignature)
	for pos+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		if length < 0 || pos+12+length > len(data) {
			return nil, fmt.Errorf("数据块 %q 越界，文件已截断", typ)
		}
		body := data[pos+8 : pos+8+length]
		want := binary.BigEndian.Uint32(data[pos+8+length : pos+12+length])
		if got := crc32.ChecksumIEEE(data[pos+4 : pos+8+length]); got != want {
			return nil, fmt.Errorf("数据块 %q CRC 校验失败", typ)
		}

		switch typ {
		case "IHDR":
			if length != 13 {
				return nil, errors.New("IHDR 长度非法")
			}
			img.w = int(binary.BigEndian.Uint32(body[0:4]))
			img.h = int(binary.BigEndian.Uint32(body[4:8]))
			img.bitDepth = int(body[8])
			img.colorType = int(body[9])
			if body[10] != 0 {
				return nil, errors.New("不支持的压缩方法")
			}
			if body[11] != 0 {
				return nil, errors.New("不支持的过滤方法")
			}
			img.interlace = int(body[12])
			seenIHDR = true
		case "PLTE":
			plte = body
		case "tRNS":
			trns = body
		case "IDAT":
			idat = append(idat, body...)
		case "IEND":
			pos = len(data)
		default:
			if _, ok := img.extras[typ]; !ok {
				img.order = append(img.order, typ)
			}
			img.extras[typ] = body
		}
		pos += 12 + length
	}

	if !seenIHDR {
		return nil, errors.New("缺少 IHDR 数据块")
	}
	if img.w <= 0 || img.h <= 0 {
		return nil, errors.New("图像尺寸非法")
	}
	if !validBitDepth(img.colorType, img.bitDepth) {
		return nil, fmt.Errorf("不支持的色彩类型 %d / 位深 %d 组合", img.colorType, img.bitDepth)
	}
	if img.interlace != 0 && img.interlace != 1 {
		return nil, errors.New("不支持的隔行方式")
	}

	// 解析调色板
	if img.colorType == ctPalette {
		if len(plte) == 0 || len(plte)%3 != 0 {
			return nil, errors.New("调色板数据块缺失或非法")
		}
		n := len(plte) / 3
		if n > 256 {
			return nil, errors.New("调色板条目超过 256")
		}
		img.palette = make([]color.NRGBA, n)
		for i := 0; i < n; i++ {
			img.palette[i] = color.NRGBA{R: plte[i*3], G: plte[i*3+1], B: plte[i*3+2], A: 255}
		}
		// tRNS：调色板 alpha 数组
		if len(trns) > 0 {
			if len(trns) > n {
				return nil, errors.New("调色板 tRNS 长度超过调色板条目数")
			}
			img.trnsPal = append([]byte(nil), trns...)
			for i, a := range img.trnsPal {
				img.palette[i].A = a
			}
		}
	} else {
		// tRNS：灰度或真彩的单个透明色
		switch img.colorType {
		case ctGray:
			if len(trns) >= 2 {
				v := binary.BigEndian.Uint16(trns[0:2])
				img.trnsGray = &v
			}
		case ctRGB:
			if len(trns) >= 6 {
				v := [3]uint16{
					binary.BigEndian.Uint16(trns[0:2]),
					binary.BigEndian.Uint16(trns[2:4]),
					binary.BigEndian.Uint16(trns[4:6]),
				}
				img.trnsRGB = &v
			}
		}
	}

	if len(idat) == 0 {
		return nil, errors.New("缺少 IDAT 数据块")
	}
	raw, err := inflateIDAT(idat)
	if err != nil {
		return nil, err
	}
	if err := img.extractSamples(raw); err != nil {
		return nil, err
	}
	return img, nil
}

// inflateIDAT 解压 IDAT 的 zlib 流
func inflateIDAT(idat []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(idat))
	if err != nil {
		return nil, fmt.Errorf("IDAT 解压失败: %w", err)
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, fmt.Errorf("IDAT 解压失败: %w", err)
	}
	return buf.Bytes(), nil
}

// adam7 七个隔行通道的起点与步长
var adam7 = [7][4]int{
	{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4},
	{0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2},
}

// extractSamples 从解压后的原始扫描线中还原每个像素的采样值
func (img *sourceImage) extractSamples(raw []byte) error {
	img.channels = channelsOf(img.colorType)
	img.samples = make([]uint16, img.w*img.h*img.channels)

	pos := 0
	if img.interlace == 0 {
		var err error
		pos, err = img.extractPass(raw, pos, 0, 0, 1, 1, img.w, img.h)
		if err != nil {
			return err
		}
	} else {
		for p := 0; p < 7; p++ {
			xs, ys, xstep, ystep := adam7[p][0], adam7[p][1], adam7[p][2], adam7[p][3]
			pw := (img.w - xs + xstep - 1) / xstep
			ph := (img.h - ys + ystep - 1) / ystep
			if pw <= 0 || ph <= 0 {
				continue
			}
			var err error
			pos, err = img.extractPass(raw, pos, xs, ys, xstep, ystep, pw, ph)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// extractPass 还原一个隔行通道（或整幅图）的扫描线
func (img *sourceImage) extractPass(raw []byte, pos, xStart, yStart, xStep, yStep, pw, ph int) (int, error) {
	bpp := img.channels * img.bitDepth
	bytesPerLine := (pw*bpp + 7) / 8
	// 过滤器的 bpp 是"字节每像素"，不足 1 字节时按 1 字节处理
	filterBPP := (bpp + 7) / 8
	if filterBPP < 1 {
		filterBPP = 1
	}
	prev := make([]byte, bytesPerLine)
	cur := make([]byte, bytesPerLine)

	for row := 0; row < ph; row++ {
		if pos+1+bytesPerLine > len(raw) {
			return pos, errors.New("扫描线数据不足，文件已截断")
		}
		ft := raw[pos]
		line := raw[pos+1 : pos+1+bytesPerLine]
		pos += 1 + bytesPerLine
		if err := unfilter(ft, line, cur, prev, bytesPerLine, filterBPP); err != nil {
			return pos, err
		}
		for x := 0; x < pw; x++ {
			fx := xStart + x*xStep
			fy := yStart + row*yStep
			if fx >= img.w || fy >= img.h {
				continue
			}
			base := (fy*img.w + fx) * img.channels
			for c := 0; c < img.channels; c++ {
				v, err := readSample(cur, x*img.channels+c, img.bitDepth)
				if err != nil {
					return pos, err
				}
				img.samples[base+c] = v
			}
		}
		// 当前行成为下一行的参考行；下一行写入另一个缓冲区
		prev, cur = cur, prev
	}
	return pos, nil
}

// readSample 从行缓冲中读取第 index 个样本（按位深）
func readSample(row []byte, index, depth int) (uint16, error) {
	switch depth {
	case 8:
		if index >= len(row) {
			return 0, errors.New("采样越界")
		}
		return uint16(row[index]), nil
	case 16:
		if index*2+2 > len(row) {
			return 0, errors.New("采样越界")
		}
		return uint16(row[index*2])<<8 | uint16(row[index*2+1]), nil
	case 1, 2, 4:
		bit := index * depth
		byteIdx := bit / 8
		if byteIdx >= len(row) {
			return 0, errors.New("采样越界")
		}
		shift := uint(8 - depth - bit%8)
		mask := byte((1 << depth) - 1)
		return uint16(row[byteIdx]>>shift) & uint16(mask), nil
	}
	return 0, fmt.Errorf("不支持的位深 %d", depth)
}

// unfilter 按 PNG 规范对一行扫描线做反向过滤
func unfilter(ft byte, line, cur, prev []byte, n, bpp int) error {
	if int(ft) > 4 {
		return fmt.Errorf("非法的过滤类型 %d", ft)
	}
	for i := 0; i < n; i++ {
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
		case 0:
			x = line[i]
		case 1:
			x = line[i] + a
		case 2:
			x = line[i] + b
		case 3:
			x = line[i] + byte((int(a)+int(b))/2)
		case 4:
			x = line[i] + paethPredictor(a, b, c)
		}
		cur[i] = x
	}
	return nil
}

// paethPredictor 是 PNG 规范中的 Paeth 预测器
func paethPredictor(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa := abs(p - int(a))
	pb := abs(p - int(b))
	pc := abs(p - int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
