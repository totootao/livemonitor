// Package media 提供媒体文件处理能力：MP3 码率解析、ffmpeg 转码、目录扫描与归档。
package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// mp3Bitrates 是 MPEG-1/2/2.5 Layer III 的比特率表（kbps）。
// 索引顺序：V1L3, V2L3（V2 与 V2.5 共用同一张表）。
var (
	mpeg1Layer3 = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mpeg2Layer3 = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
)

var (
	mpeg1SampleRates  = []int{44100, 48000, 32000, 0}
	mpeg2SampleRates  = []int{22050, 24000, 16000, 0}
	mpeg25SampleRates = []int{11025, 12000, 8000, 0}
)

// ErrNoMP3Frame 表示在给定范围内未找到有效的 MP3 帧头。
var ErrNoMP3Frame = errors.New("未找到有效的 MP3 帧头")

// MP3Info 描述 MP3 文件的音频参数。
type MP3Info struct {
	// BitrateKbps 是常量码率（kbps），VBR 文件取首帧码率。
	BitrateKbps int
	// SampleRate 是采样率（Hz）。
	SampleRate int
	// Channels 是声道数。
	Channels int
	// Version 形如 "MPEG-1"/"MPEG-2"/"MPEG-2.5"。
	Version string
	// Layer 为层号（3 表示 Layer III）。
	Layer int
	// VBR 表示首帧是否为 Xing/Info/VBRI 头，通常意味着变码率。
	VBR bool
}

// IsLowQuality 判断码率是否低于给定阈值（kbps）。
func (i MP3Info) IsLowQuality(threshold int) bool {
	return i.BitrateKbps < threshold
}

// ProbeMP3 读取 MP3 文件的帧头并解析音频参数。
// 实现等价于 Python mutagen 的 MP3(file).info.bitrate // 1000：
// 扫描文件前部找到同步字，解析帧头得到码率等字段。
// 这样避免了对 ID3 标签库的完整依赖，同时保留了 ID3v2 跳过的能力。
func ProbeMP3(path string) (MP3Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return MP3Info{}, err
	}
	defer f.Close()

	skip, err := id3v2Size(f)
	if err != nil {
		return MP3Info{}, err
	}
	if _, err := f.Seek(skip, io.SeekStart); err != nil {
		return MP3Info{}, err
	}

	// 扫描范围：跳过 ID3 后最多读取 1MB，足够命中首帧。
	const maxScan = 1 << 20
	buf := make([]byte, 64*1024)
	var window []byte
	total := int64(0)

	for total < maxScan {
		n, readErr := f.Read(buf)
		if n > 0 {
			window = append(window, buf[:n]...)
			total += int64(n)
		}
		for i := 0; i+4 <= len(window); i++ {
			if window[i] != 0xFF || window[i+1]&0xE0 != 0xE0 {
				continue
			}
			info, ok := parseFrameHeader(window[i : i+4])
			if !ok {
				continue
			}
			// 用第二帧位置做一次一致性校验，避免误命中 id3 数据里的伪同步字。
			if info.VBR {
				return info, nil
			}
			next := i + frameLength(info)
			if next+4 <= len(window) {
				if _, ok2 := parseFrameHeader(window[next : next+4]); ok2 {
					return info, nil
				}
			} else {
				// 缓冲区不够做二次校验，直接返回首帧结果。
				return info, nil
			}
		}
		// 仅保留末尾 3 字节用于跨读取边界的同步字匹配。
		if len(window) > 3 {
			window = window[len(window)-3:]
		}
		if readErr != nil {
			break
		}
	}
	return MP3Info{}, ErrNoMP3Frame
}

// id3v2Size 返回文件开头 ID3v2 标签的总长度（含 header 与 footer）。
func id3v2Size(r io.ReadSeeker) (int64, error) {
	header := make([]byte, 10)
	if _, err := io.ReadFull(r, header); err != nil {
		// 文件过短，视为无标签。
		if _, seekErr := r.Seek(0, io.SeekStart); seekErr != nil {
			return 0, seekErr
		}
		return 0, nil
	}
	if string(header[0:3]) != "ID3" {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		return 0, nil
	}
	// syncsafe integer：每字节 7 位有效。
	size := int64(header[6]&0x7F)<<21 | int64(header[7]&0x7F)<<14 | int64(header[8]&0x7F)<<7 | int64(header[9]&0x7F)
	size += 10
	if header[5]&0x10 != 0 { // footer present
		size += 10
	}
	return size, nil
}

// frameLength 按码率与采样率计算帧字节长度。
func frameLength(info MP3Info) int {
	if info.SampleRate == 0 || info.BitrateKbps == 0 {
		return 0
	}
	var coef int
	if info.Version == "MPEG-1" {
		coef = 144
	} else {
		coef = 72
	}
	return coef*info.BitrateKbps*1000/info.SampleRate + 1
}

// parseFrameHeader 解析 4 字节帧头。
func parseFrameHeader(b []byte) (MP3Info, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return MP3Info{}, false
	}

	versionID := (b[1] >> 3) & 0x03
	layerID := (b[1] >> 1) & 0x03
	if versionID == 1 || layerID == 0 {
		return MP3Info{}, false // 保留值 / 保留层
	}

	bitrateIdx := (b[2] >> 4) & 0x0F
	sampleIdx := (b[2] >> 2) & 0x03
	channelMode := (b[3] >> 6) & 0x03

	if bitrateIdx == 0 || bitrateIdx == 15 || sampleIdx == 3 {
		return MP3Info{}, false // free/bad 码率或保留采样率
	}

	var version string
	var bitrates []int
	var sampleRates []int
	switch versionID {
	case 0: // MPEG-2.5
		version = "MPEG-2.5"
		bitrates = mpeg2Layer3
		sampleRates = mpeg25SampleRates
	case 2: // MPEG-2
		version = "MPEG-2"
		bitrates = mpeg2Layer3
		sampleRates = mpeg2SampleRates
	default: // MPEG-1
		version = "MPEG-1"
		bitrates = mpeg1Layer3
		sampleRates = mpeg1SampleRates
	}

	// 仅处理 Layer III（layerID == 1）。
	if layerID != 1 {
		return MP3Info{}, false
	}

	br := 0
	if int(bitrateIdx) < len(bitrates) {
		br = bitrates[bitrateIdx]
	}
	sr := 0
	if int(sampleIdx) < len(sampleRates) {
		sr = sampleRates[sampleIdx]
	}
	if br == 0 || sr == 0 {
		return MP3Info{}, false
	}

	channels := 2
	if channelMode == 3 {
		channels = 1
	}

	return MP3Info{
		BitrateKbps: br,
		SampleRate:  sr,
		Channels:    channels,
		Version:     version,
		Layer:       3,
		VBR:         false, // 由调用方结合 Xing 头进一步判断
	}, true
}

// HasXingHeader 检测帧内是否含 Xing/Info/VBRI 头（表示 VBR）。
func HasXingHeader(frame []byte) bool {
	if len(frame) < 40 {
		return false
	}
	for _, tag := range []string{"Xing", "Info", "VBRI"} {
		if binary.BigEndian.Uint32(frame[36:40]) == binary.BigEndian.Uint32([]byte(tag)) {
			return true
		}
	}
	return false
}

// Describe 返回可读的音频描述。
func (i MP3Info) Describe() string {
	return fmt.Sprintf("%s Layer III, %d kbps, %d Hz, %d 声道", i.Version, i.BitrateKbps, i.SampleRate, i.Channels)
}
