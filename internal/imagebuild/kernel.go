package imagebuild

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
)

// VZLinuxBootLoader 在 arm64 上只接受未压缩的 Image。Alpine 的 vmlinuz-virt
// 是 EFI zboot 格式（CONFIG_EFI_ZBOOT=y）：一个 PE 外壳，里面是 gzip 压缩的 Image。
//
// zboot 头（drivers/firmware/efi/libstub/zboot-header.S）：
//   偏移 0   "MZ"
//   偏移 4   "zimg"
//   偏移 8   压缩数据的起始偏移（uint32 小端）
//   偏移 12  压缩数据的长度（uint32 小端）
//   偏移 24  压缩类型，以 NUL 结尾的字符串（如 "gzip"）

const maxKernelSize = 256 << 20

// arm64Magic 是 arm64 Image 头里偏移 56 处的魔数 "ARM\x64"。
var arm64Magic = []byte{'A', 'R', 'M', 0x64}

// IsArm64Image 检查是否是未压缩的 arm64 Image。
func IsArm64Image(b []byte) bool {
	return len(b) >= 64 && bytes.Equal(b[56:60], arm64Magic)
}

// ExtractImage 把内核文件解成未压缩的 arm64 Image。支持：已经是 Image、
// 纯 gzip、gzip 压缩的 EFI zboot。其他格式明确报错。
func ExtractImage(b []byte) ([]byte, error) {
	switch {
	case IsArm64Image(b):
		return b, nil
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		return gunzipImage(b)
	case len(b) >= 64 && string(b[0:2]) == "MZ" && string(b[4:8]) == "zimg":
		off := binary.LittleEndian.Uint32(b[8:12])
		size := binary.LittleEndian.Uint32(b[12:16])
		comp := string(bytes.TrimRight(b[24:56], "\x00"))
		if i := bytes.IndexByte(b[24:56], 0); i >= 0 {
			comp = string(b[24 : 24+i])
		}
		if uint64(off)+uint64(size) > uint64(len(b)) || off == 0 {
			return nil, fmt.Errorf("zboot 头里的压缩数据位置超出文件范围")
		}
		if comp != "gzip" {
			return nil, fmt.Errorf("不支持的 zboot 压缩类型 %q（目前只支持 gzip）", comp)
		}
		return gunzipImage(b[off:])
	}
	return nil, fmt.Errorf("无法识别的内核格式（既不是 arm64 Image，也不是 gzip 或 EFI zboot）")
}

func gunzipImage(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("解压内核：%w", err)
	}
	zr.Multistream(false)
	img, err := io.ReadAll(io.LimitReader(zr, maxKernelSize+1))
	if err != nil {
		return nil, fmt.Errorf("解压内核：%w", err)
	}
	if len(img) > maxKernelSize {
		return nil, fmt.Errorf("解压后的内核超过 %d MiB", maxKernelSize>>20)
	}
	if !IsArm64Image(img) {
		return nil, fmt.Errorf("解压后不是 arm64 Image（偏移 56 处没有 ARM\\x64 魔数）")
	}
	return img, nil
}
