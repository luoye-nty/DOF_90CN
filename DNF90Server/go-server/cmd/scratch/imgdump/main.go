// imgdump extracts frames from an IMG inside a DNF NPK and writes them as PNG,
// so sprite art can be inspected directly (is a title baked into the texture?).
//
// DNF IMG v2 frames: type 0x10 = 32bpp BGRA, 0x0E = 16bpp, 0x11 = LINK.
// compression 0x05 = stored, 0x06 = zlib.
//
//	go run ./cmd/scratch/imgdump -npk <x.NPK> -img selectdungeon02.img -list
//	go run ./cmd/scratch/imgdump -npk <x.NPK> -img selectdungeon02.img -frames 3,7,10,13
//	go run ./cmd/scratch/imgdump -npk <x.NPK> -img selectdungeon02.img -minw 50
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const npkMagic = "NeoplePack_Bill\x00"

var npkNameKey = []byte("puchikon@neople dungeon and fighter " +
	"DNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNF\x00")

type npkEntry struct {
	Name   string
	Offset uint32
	Length uint32
}

type imgFrame struct {
	Index       int
	Type        int32
	Compression int32
	Width       int32
	Height      int32
	Length      int32
	X           int32
	Y           int32
	CanvasW     int32
	CanvasH     int32
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func readNPKIndex(path string) ([]npkEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 20 || string(data[:16]) != npkMagic {
		return nil, fmt.Errorf("bad NPK magic in %s", path)
	}
	count := int(binary.LittleEndian.Uint32(data[16:20]))
	out := make([]npkEntry, 0, count)
	for i := 0; i < count; i++ {
		row := data[20+264*i : 20+264*(i+1)]
		nameBytes := append([]byte(nil), row[8:]...)
		for p := range nameBytes {
			nameBytes[p] ^= npkNameKey[p]
		}
		name := string(nameBytes)
		if z := strings.IndexByte(name, 0); z >= 0 {
			name = name[:z]
		}
		out = append(out, npkEntry{
			Offset: binary.LittleEndian.Uint32(row[0:4]),
			Length: binary.LittleEndian.Uint32(row[4:8]),
			Name:   strings.TrimSpace(name),
		})
	}
	return out, nil
}

func parseIMG(blob []byte) ([]imgFrame, int, error) {
	if len(blob) < 32 || !strings.HasPrefix(string(blob[:16]), "Neople Img File") {
		return nil, 0, fmt.Errorf("unsupported IMG magic")
	}
	count := int(binary.LittleEndian.Uint32(blob[28:32]))
	if count <= 0 || count > 200000 {
		return nil, 0, fmt.Errorf("unreasonable frame count %d", count)
	}
	frames := make([]imgFrame, 0, count)
	off := 32
	for i := 0; i < count; i++ {
		if off+8 > len(blob) {
			return nil, 0, fmt.Errorf("frame %d header out of range", i)
		}
		typ := int32(binary.LittleEndian.Uint32(blob[off : off+4]))
		if typ == 0x11 {
			frames = append(frames, imgFrame{Index: i, Type: typ})
			off += 8
			continue
		}
		if off+36 > len(blob) {
			return nil, 0, fmt.Errorf("frame %d metadata out of range", i)
		}
		r := func(s int) int32 { return int32(binary.LittleEndian.Uint32(blob[off+s : off+s+4])) }
		frames = append(frames, imgFrame{
			Index: i, Type: typ, Compression: r(4), Width: r(8), Height: r(12),
			Length: r(16), X: r(20), Y: r(24), CanvasW: r(28), CanvasH: r(32),
		})
		off += 36
	}
	return frames, off, nil
}

func decodeBGRA(data []byte, w, h, bpp int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * bpp
			if i+bpp > len(data) {
				return img
			}
			if bpp == 4 {
				img.SetNRGBA(x, y, color.NRGBA{R: data[i+2], G: data[i+1], B: data[i], A: data[i+3]})
				continue
			}
			// 16bpp: 4 bits per channel, little-endian (ARGB4444)
			v := binary.LittleEndian.Uint16(data[i : i+2])
			img.SetNRGBA(x, y, color.NRGBA{
				A: uint8((v>>12)&0xF) * 17,
				R: uint8((v>>8)&0xF) * 17,
				G: uint8((v>>4)&0xF) * 17,
				B: uint8(v&0xF) * 17,
			})
		}
	}
	return img
}

// framePixels decodes one frame, honouring comp 0x05 (stored) / 0x06 (zlib).
func framePixels(blob []byte, fr imgFrame, off int) (*image.NRGBA, error) {
	w, h := int(fr.Width), int(fr.Height)
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return nil, fmt.Errorf("bad size %dx%d", w, h)
	}
	if fr.Length <= 0 || off+int(fr.Length) > len(blob) {
		return nil, fmt.Errorf("frame data out of range")
	}
	data := blob[off : off+int(fr.Length)]
	if fr.Compression == 0x06 {
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("zlib: %w", err)
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("zlib read: %w", err)
		}
		data = raw
	}
	switch fr.Type {
	case 0x10:
		if len(data) < w*h*4 {
			return nil, fmt.Errorf("short BGRA %d < %d", len(data), w*h*4)
		}
		return decodeBGRA(data, w, h, 4), nil
	case 0x0E, 0x0F:
		if len(data) < w*h*2 {
			return nil, fmt.Errorf("short 16bpp %d < %d", len(data), w*h*2)
		}
		return decodeBGRA(data, w, h, 2), nil
	}
	return nil, fmt.Errorf("unsupported type 0x%02X", fr.Type)
}

func main() {
	npkPath := flag.String("npk", "", "NPK file")
	imgMatch := flag.String("img", "", "IMG member name substring")
	listOnly := flag.Bool("list", false, "only list frame headers")
	framesFlag := flag.String("frames", "", "comma-separated frame indices to extract")
	minW := flag.Int("minw", 0, "extract every frame at least this wide")
	outDir := flag.String("out", ".", "output directory for PNGs")
	flag.Parse()

	entries, err := readNPKIndex(*npkPath)
	fail(err)
	var found *npkEntry
	for i := range entries {
		if strings.Contains(strings.ToLower(entries[i].Name), strings.ToLower(*imgMatch)) {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		fail(fmt.Errorf("IMG %q not found", *imgMatch))
	}
	fmt.Printf("npk=%s img=%s offset=%d length=%d\n",
		filepath.Base(*npkPath), found.Name, found.Offset, found.Length)

	f, err := os.Open(*npkPath)
	fail(err)
	defer f.Close()
	blob := make([]byte, found.Length)
	_, err = f.ReadAt(blob, int64(found.Offset))
	fail(err)

	frames, dataStart, err := parseIMG(blob)
	fail(err)
	fmt.Printf("frames=%d dataStart=%d\n", len(frames), dataStart)

	offsets := make([]int, len(frames))
	cur := dataStart
	for i, fr := range frames {
		offsets[i] = cur
		if fr.Type != 0x11 {
			cur += int(fr.Length)
		}
	}

	if *listOnly {
		for _, fr := range frames {
			if fr.Type == 0x11 {
				fmt.Printf("  frame=%3d LINK\n", fr.Index)
				continue
			}
			fmt.Printf("  frame=%3d type=0x%02X comp=0x%02X %dx%d bytes=%d at=%d,%d canvas=%dx%d\n",
				fr.Index, fr.Type, fr.Compression, fr.Width, fr.Height, fr.Length,
				fr.X, fr.Y, fr.CanvasW, fr.CanvasH)
		}
		return
	}

	want := map[int]bool{}
	for _, part := range strings.Split(*framesFlag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx, err := strconv.Atoi(part); err == nil {
			want[idx] = true
		}
	}
	if *minW > 0 {
		for _, fr := range frames {
			if fr.Type != 0x11 && int(fr.Width) >= *minW {
				want[fr.Index] = true
			}
		}
	}

	fail(os.MkdirAll(*outDir, 0o755))
	indices := make([]int, 0, len(want))
	for idx := range want {
		indices = append(indices, idx)
	}
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] < indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
	ok, skipped := 0, 0
	for _, idx := range indices {
		if idx < 0 || idx >= len(frames) {
			continue
		}
		fr := frames[idx]
		if fr.Type == 0x11 {
			continue
		}
		img, err := framePixels(blob, fr, offsets[idx])
		if err != nil {
			fmt.Printf("  frame=%d skipped: %v\n", idx, err)
			skipped++
			continue
		}
		name := filepath.Join(*outDir, fmt.Sprintf("f%03d_%dx%d.png", idx, fr.Width, fr.Height))
		out, err := os.Create(name)
		fail(err)
		fail(png.Encode(out, img))
		out.Close()
		ok++
	}
	fmt.Printf("wrote %d PNG(s), skipped %d, into %s\n", ok, skipped, *outDir)
}
