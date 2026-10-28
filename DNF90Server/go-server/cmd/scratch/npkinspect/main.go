// npkinspect prints the IMG index and basic frame metadata from DNF NPK files.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const npkMagic = "NeoplePack_Bill\x00"

var npkNameKey = []byte("puchikon@neople dungeon and fighter " +
	"DNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNF\x00")

type entry struct {
	Name   string
	Offset uint32
	Length uint32
}

type spriteEntry struct {
	Index       int
	Type        int32
	Target      int32
	Compression int32
	Width       int32
	Height      int32
	Length      int32
	X           int32
	Y           int32
	FrameWidth  int32
	FrameHeight int32
}

func main() {
	npkPath := flag.String("npk", "", "one NPK file")
	directory := flag.String("dir", "", "scan an ImagePacks2 directory")
	match := flag.String("match", "", "case-insensitive IMG path filter")
	includeFrames := flag.Bool("frames", false, "print every sprite entry for matched IMG members")
	includeHashes := flag.Bool("hashes", false, "print SHA-256 of every matched IMG member")
	flag.Parse()
	if strings.TrimSpace(*npkPath) == "" && strings.TrimSpace(*directory) == "" {
		fmt.Fprintln(os.Stderr, "-npk or -dir is required")
		os.Exit(2)
	}
	paths := []string{*npkPath}
	if strings.TrimSpace(*directory) != "" {
		paths = nil
		matches, err := filepath.Glob(filepath.Join(*directory, "*.NPK"))
		if err != nil {
			panic(err)
		}
		paths = matches
	}
	filter := strings.ToLower(strings.TrimSpace(*match))
	for _, filePath := range paths {
		if strings.TrimSpace(filePath) == "" {
			continue
		}
		entries, err := readNPK(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", filePath, err)
			continue
		}
		for _, item := range entries {
			if filter != "" && !strings.Contains(strings.ToLower(item.Name), filter) {
				continue
			}
			version, frameCount, imageMagic, metadataErr := readIMGMetadata(filePath, item)
			if metadataErr != nil {
				fmt.Printf("npk=%s img=%s offset=%d length=%d metadata_error=%q\n", filepath.Base(filePath), item.Name, item.Offset, item.Length, metadataErr)
				continue
			}
			memberHash := ""
			if *includeHashes {
				hash, hashErr := readIMGHash(filePath, item)
				if hashErr != nil {
					fmt.Printf("npk=%s img=%s offset=%d length=%d hash_error=%q\n", filepath.Base(filePath), item.Name, item.Offset, item.Length, hashErr)
					continue
				}
				memberHash = fmt.Sprintf(" sha256=%x", hash)
			}
			fmt.Printf("npk=%s img=%s offset=%d length=%d magic=%q version=%d frames=%d%s\n", filepath.Base(filePath), item.Name, item.Offset, item.Length, imageMagic, version, frameCount, memberHash)
			if *includeFrames {
				sprites, spriteErr := readIMGFrames(filePath, item)
				if spriteErr != nil {
					fmt.Printf("  frame_error=%q\n", spriteErr)
					continue
				}
				for _, sprite := range sprites {
					if sprite.Type == 0x11 {
						fmt.Printf("  frame=%d type=LINK target=%d\n", sprite.Index, sprite.Target)
						continue
					}
					fmt.Printf("  frame=%d type=0x%02X compression=0x%02X size=%dx%d bytes=%d offset=%d,%d canvas=%dx%d\n",
						sprite.Index, sprite.Type, sprite.Compression, sprite.Width, sprite.Height,
						sprite.Length, sprite.X, sprite.Y, sprite.FrameWidth, sprite.FrameHeight)
				}
			}
		}
	}
}

func readIMGHash(filePath string, item entry) ([32]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return [32]byte{}, err
	}
	defer file.Close()
	data := make([]byte, item.Length)
	if _, err := file.ReadAt(data, int64(item.Offset)); err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func readIMGFrames(filePath string, item entry) ([]spriteEntry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data := make([]byte, item.Length)
	if _, err := file.ReadAt(data, int64(item.Offset)); err != nil {
		return nil, err
	}
	if len(data) < 32 || !strings.HasPrefix(string(data[:16]), "Neople Img File") {
		return nil, fmt.Errorf("frame audit supports only the 32-byte IMG header")
	}
	count := int(binary.LittleEndian.Uint32(data[28:32]))
	if count < 0 || count > 1_000_000 {
		return nil, fmt.Errorf("unreasonable frame count %d", count)
	}
	offset := 32
	sprites := make([]spriteEntry, 0, count)
	for index := 0; index < count; index++ {
		if offset+8 > len(data) {
			return nil, fmt.Errorf("frame %d header exceeds IMG member", index)
		}
		spriteType := int32(binary.LittleEndian.Uint32(data[offset : offset+4]))
		if spriteType == 0x11 {
			sprites = append(sprites, spriteEntry{
				Index:  index,
				Type:   spriteType,
				Target: int32(binary.LittleEndian.Uint32(data[offset+4 : offset+8])),
			})
			offset += 8
			continue
		}
		if offset+36 > len(data) {
			return nil, fmt.Errorf("frame %d metadata exceeds IMG member", index)
		}
		readI32 := func(start int) int32 {
			return int32(binary.LittleEndian.Uint32(data[offset+start : offset+start+4]))
		}
		sprites = append(sprites, spriteEntry{
			Index:       index,
			Type:        spriteType,
			Compression: readI32(4),
			Width:       readI32(8),
			Height:      readI32(12),
			Length:      readI32(16),
			X:           readI32(20),
			Y:           readI32(24),
			FrameWidth:  readI32(28),
			FrameHeight: readI32(32),
		})
		offset += 36
	}
	return sprites, nil
}

func readNPK(filePath string) ([]entry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, err
	}
	if string(header[:16]) != npkMagic {
		return nil, fmt.Errorf("invalid magic %q", header[:16])
	}
	count := binary.LittleEndian.Uint32(header[16:20])
	if count > 1_000_000 {
		return nil, fmt.Errorf("unreasonable IMG count %d", count)
	}
	entries := make([]entry, 0, count)
	for index := uint32(0); index < count; index++ {
		row := make([]byte, 264)
		if _, err := io.ReadFull(file, row); err != nil {
			return nil, fmt.Errorf("read index %d: %w", index, err)
		}
		nameBytes := append([]byte(nil), row[8:]...)
		for position := range nameBytes {
			nameBytes[position] ^= npkNameKey[position]
		}
		name := string(nameBytes)
		if zero := strings.IndexByte(name, 0); zero >= 0 {
			name = name[:zero]
		}
		entries = append(entries, entry{
			Offset: binary.LittleEndian.Uint32(row[0:4]),
			Length: binary.LittleEndian.Uint32(row[4:8]),
			Name:   strings.TrimSpace(name),
		})
	}
	return entries, nil
}

func readIMGMetadata(filePath string, item entry) (uint32, uint32, string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, 0, "", err
	}
	defer file.Close()
	if _, err := file.Seek(int64(item.Offset), io.SeekStart); err != nil {
		return 0, 0, "", err
	}
	reader := bufio.NewReader(file)
	magic, err := reader.ReadString(0)
	if err != nil {
		return 0, 0, "", err
	}
	magic = strings.TrimSuffix(magic, "\x00")
	var metadata [16]byte
	if _, err := io.ReadFull(reader, metadata[:]); err != nil {
		return 0, 0, magic, err
	}
	if strings.HasPrefix(magic, "Neople Image File") {
		// Legacy form uses a two-byte reserved field, so its version/count are
		// not aligned with the newer four-int header.
		version := binary.LittleEndian.Uint32(metadata[6:10])
		frames := binary.LittleEndian.Uint32(metadata[10:14])
		return version, frames, magic, nil
	}
	if strings.HasPrefix(magic, "Neople Img File") {
		return binary.LittleEndian.Uint32(metadata[8:12]), binary.LittleEndian.Uint32(metadata[12:16]), magic, nil
	}
	return 0, 0, magic, fmt.Errorf("unsupported IMG magic")
}
