package pvf

import (
	"encoding/binary"
	"fmt"
)

// PatchFileDataType 把文件表里某个已存在表项的 dataType 字段改写为 want，
// 并返回重建后的完整归档字节。
//
// 为什么可以只改 4 字节：文件表本身是明文（parseNKPITables 直接把 a.data 当表读），
// 表项布局 = nameOffset/pathOffset/chunkIndex/dataOffset/dataSize/dataType 六个
// int32（见 parse.go 的 parseFiles），dataType 在记录 +20 处。数据体、字符串池、
// 分组表、体偏移都不需要动，所以这是定点写，不会重排归档。
//
// 现场用途（2026-09-19）：玩家用第三方工具新增 stackable/10000001/_test123.stk 时，
// 新文件被以 dataType=3 写入；本仓库 patch_string.go 的判据与客户端一致——.stk 这类
// 脚本文档必须是 type 1，于是客户端解析不了这个新道具（改现有道具不受影响，因为
// 原有文件仍是 type 1）。把该表项改回 1 即可让新道具在客户端可见。
func (a *Archive) PatchFileDataType(relativePath string, want int) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil archive", ErrInvalidArchive)
	}
	key := pathKey(relativePath)
	fileIndex, found := a.pathIdx[key]
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, relativePath)
	}
	off := a.tableOff + fileIndex*fileItemSize + 20
	if a.tableOff <= 0 || off+4 > len(a.data) {
		return nil, fmt.Errorf("%w: file table offset out of range", ErrInvalidArchive)
	}

	out := append([]byte(nil), a.data...)
	binary.LittleEndian.PutUint32(out[off:off+4], uint32(want))

	verified, err := OpenBytes(out)
	if err != nil {
		return nil, fmt.Errorf("reopen patched archive: %w", err)
	}
	for _, file := range verified.Files() {
		if pathKey(file.ArchivePath) != key {
			continue
		}
		if file.DataType != want {
			return nil, fmt.Errorf("patch verification failed: %s dataType=%d want=%d",
				relativePath, file.DataType, want)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrFileNotFound, relativePath)
}
