// npkmerge rebuilds a base NPK while replacing or adding selected whole IMG
// members from a source NPK. It recalculates the NPK index checksum.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const npkHeader = "NeoplePack_Bill\x00"

var npkNameKey = []byte("puchikon@neople dungeon and fighter " +
	"DNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNFDNF\x00")

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error {
	*l = append(*l, normalize(value))
	return nil
}

type imageReplacement struct {
	Target string
	Source string
}

type replacementList []imageReplacement

func (l *replacementList) String() string {
	parts := make([]string, 0, len(*l))
	for _, replacement := range *l {
		parts = append(parts, replacement.Target+"="+replacement.Source)
	}
	return strings.Join(parts, ",")
}

func (l *replacementList) Set(value string) error {
	target, source, found := strings.Cut(value, "=")
	if !found {
		return fmt.Errorf("replacement must use target=source")
	}
	target = normalize(target)
	source = normalize(source)
	if target == "" || source == "" {
		return fmt.Errorf("replacement target and source are required")
	}
	*l = append(*l, imageReplacement{Target: target, Source: source})
	return nil
}

type member struct {
	Name string
	Data []byte
}

func main() {
	basePath := flag.String("base", "", "base NPK")
	sourcePath := flag.String("source", "", "source NPK")
	outputPath := flag.String("out", "", "output NPK")
	appendV2From := flag.Int("append-v2-from", -1, "preserve base IMG frames and append source IMG v2 frames starting at this index")
	var selected stringList
	var replacements replacementList
	flag.Var(&selected, "img", "IMG path to replace or add (repeatable)")
	flag.Var(&replacements, "replace", "replace an existing base IMG with a differently named source IMG: target=source (repeatable)")
	flag.Parse()
	if *basePath == "" || *sourcePath == "" || *outputPath == "" || len(selected) == 0 && len(replacements) == 0 {
		fmt.Fprintln(os.Stderr, "-base, -source, -out and at least one -img or -replace are required")
		os.Exit(2)
	}
	baseAbs, err := filepath.Abs(*basePath)
	if err != nil {
		panic(err)
	}
	sourceAbs, err := filepath.Abs(*sourcePath)
	if err != nil {
		panic(err)
	}
	outputAbs, err := filepath.Abs(*outputPath)
	if err != nil {
		panic(err)
	}
	if strings.EqualFold(baseAbs, outputAbs) || strings.EqualFold(sourceAbs, outputAbs) {
		panic("output must differ from base and source")
	}
	base, err := readMembers(baseAbs)
	if err != nil {
		panic(err)
	}
	source, err := readMembers(sourceAbs)
	if err != nil {
		panic(err)
	}
	sourceByName := make(map[string]member, len(source))
	for _, item := range source {
		sourceByName[normalize(item.Name)] = item
	}
	baseByName := make(map[string]int, len(base))
	for index, item := range base {
		baseByName[normalize(item.Name)] = index
	}
	if *appendV2From >= 0 && len(replacements) != 0 {
		panic("-append-v2-from cannot be combined with differently named -replace entries")
	}
	for _, replacement := range replacements {
		sourceItem, found := sourceByName[replacement.Source]
		if !found {
			panic(fmt.Sprintf("replacement source IMG %s is absent from source", replacement.Source))
		}
		index, found := baseByName[replacement.Target]
		if !found {
			panic(fmt.Sprintf("replacement target IMG %s is absent from base", replacement.Target))
		}
		base[index] = member{Name: base[index].Name, Data: sourceItem.Data}
	}
	wanted := make(map[string]struct{}, len(selected))
	for _, name := range selected {
		if _, found := sourceByName[name]; !found {
			panic(fmt.Sprintf("selected IMG %s is absent from source", name))
		}
		wanted[name] = struct{}{}
	}
	replaced := make(map[string]struct{})
	for index := range base {
		name := normalize(base[index].Name)
		if _, ok := wanted[name]; !ok {
			continue
		}
		if *appendV2From >= 0 {
			merged, mergeErr := appendIMGv2Frames(base[index].Data, sourceByName[name].Data, *appendV2From)
			if mergeErr != nil {
				panic(fmt.Sprintf("append IMG frames for %s: %v", name, mergeErr))
			}
			base[index] = member{Name: base[index].Name, Data: merged}
		} else {
			base[index] = sourceByName[name]
		}
		replaced[name] = struct{}{}
	}
	for _, name := range selected {
		if _, ok := replaced[name]; ok {
			continue
		}
		if *appendV2From >= 0 {
			panic(fmt.Sprintf("cannot append frames because base IMG %s is absent", name))
		}
		base = append(base, sourceByName[name])
	}
	output, err := encodeNPK(base)
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(outputAbs, output, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("base=%s source=%s output=%s members=%d bytes=%d sha256=%X\n", baseAbs, sourceAbs, outputAbs, len(base), len(output), sha256.Sum256(output))
	for _, replacement := range replacements {
		fmt.Printf("replaced=%s source=%s bytes=%d\n", replacement.Target, replacement.Source, len(sourceByName[replacement.Source].Data))
	}
	for _, name := range selected {
		fmt.Printf("selected=%s bytes=%d\n", name, len(sourceByName[name].Data))
	}
}

type imgV2Frame struct {
	indexOffset int
	indexLength int
	dataOffset  int
	dataLength  int
}

type imgV2Layout struct {
	count     int
	dataStart int
	dataEnd   int
	frames    []imgV2Frame
}

func appendIMGv2Frames(baseData, sourceData []byte, start int) ([]byte, error) {
	base, err := parseIMGv2Layout(baseData)
	if err != nil {
		return nil, fmt.Errorf("parse base: %w", err)
	}
	source, err := parseIMGv2Layout(sourceData)
	if err != nil {
		return nil, fmt.Errorf("parse source: %w", err)
	}
	if start != base.count {
		return nil, fmt.Errorf("append index %d must equal base frame count %d", start, base.count)
	}
	if start < 0 || start >= source.count {
		return nil, fmt.Errorf("append index %d outside source frame count %d", start, source.count)
	}
	if base.dataEnd != len(baseData) || source.dataEnd != len(sourceData) {
		return nil, fmt.Errorf("trailing IMG bytes are unsupported: base=%d source=%d", len(baseData)-base.dataEnd, len(sourceData)-source.dataEnd)
	}
	extraIndexStart := source.frames[start].indexOffset
	extraDataStart := source.frames[start].dataOffset
	baseIndex := baseData[32:base.dataStart]
	extraIndex := sourceData[extraIndexStart:source.dataStart]
	basePixels := baseData[base.dataStart:base.dataEnd]
	extraPixels := sourceData[extraDataStart:source.dataEnd]
	output := make([]byte, 32, 32+len(baseIndex)+len(extraIndex)+len(basePixels)+len(extraPixels))
	copy(output, baseData[:32])
	binary.LittleEndian.PutUint64(output[16:24], uint64(len(baseIndex)+len(extraIndex)))
	binary.LittleEndian.PutUint32(output[28:32], uint32(base.count+source.count-start))
	output = append(output, baseIndex...)
	output = append(output, extraIndex...)
	output = append(output, basePixels...)
	output = append(output, extraPixels...)
	if _, err := parseIMGv2Layout(output); err != nil {
		return nil, fmt.Errorf("validate merged IMG: %w", err)
	}
	return output, nil
}

func parseIMGv2Layout(data []byte) (imgV2Layout, error) {
	if len(data) < 32 || string(data[:16]) != "Neople Img File\x00" {
		return imgV2Layout{}, fmt.Errorf("not a 32-byte-header IMG")
	}
	if version := binary.LittleEndian.Uint32(data[24:28]); version != 2 {
		return imgV2Layout{}, fmt.Errorf("unsupported IMG version %d", version)
	}
	indexLength64 := binary.LittleEndian.Uint64(data[16:24])
	if indexLength64 > uint64(len(data)-32) {
		return imgV2Layout{}, fmt.Errorf("index length %d exceeds member bytes %d", indexLength64, len(data))
	}
	count := int(binary.LittleEndian.Uint32(data[28:32]))
	if count < 0 || count > 1_000_000 {
		return imgV2Layout{}, fmt.Errorf("unreasonable frame count %d", count)
	}
	dataStart := 32 + int(indexLength64)
	indexOffset := 32
	dataOffset := dataStart
	frames := make([]imgV2Frame, 0, count)
	for index := 0; index < count; index++ {
		if indexOffset+8 > dataStart {
			return imgV2Layout{}, fmt.Errorf("frame %d header exceeds index", index)
		}
		frameType := binary.LittleEndian.Uint32(data[indexOffset : indexOffset+4])
		if frameType == 0x11 {
			frames = append(frames, imgV2Frame{indexOffset: indexOffset, indexLength: 8, dataOffset: dataOffset})
			indexOffset += 8
			continue
		}
		if indexOffset+36 > dataStart {
			return imgV2Layout{}, fmt.Errorf("frame %d metadata exceeds index", index)
		}
		compression := binary.LittleEndian.Uint32(data[indexOffset+4 : indexOffset+8])
		width := uint64(binary.LittleEndian.Uint32(data[indexOffset+8 : indexOffset+12]))
		height := uint64(binary.LittleEndian.Uint32(data[indexOffset+12 : indexOffset+16]))
		storedLength := uint64(binary.LittleEndian.Uint32(data[indexOffset+16 : indexOffset+20]))
		if compression == 0x05 {
			switch frameType {
			case 0x0e, 0x0f:
				storedLength = width * height * 2
			case 0x10:
				storedLength = width * height * 4
			}
		}
		if storedLength > uint64(len(data)-dataOffset) {
			return imgV2Layout{}, fmt.Errorf("frame %d data length %d exceeds member", index, storedLength)
		}
		frames = append(frames, imgV2Frame{
			indexOffset: indexOffset,
			indexLength: 36,
			dataOffset:  dataOffset,
			dataLength:  int(storedLength),
		})
		indexOffset += 36
		dataOffset += int(storedLength)
	}
	if indexOffset != dataStart {
		return imgV2Layout{}, fmt.Errorf("parsed index ends at %d, header data starts at %d", indexOffset, dataStart)
	}
	return imgV2Layout{count: count, dataStart: dataStart, dataEnd: dataOffset, frames: frames}, nil
}

func readMembers(filePath string) ([]member, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var header [20]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, err
	}
	if string(header[:16]) != npkHeader {
		return nil, fmt.Errorf("invalid NPK magic")
	}
	count := binary.LittleEndian.Uint32(header[16:])
	type indexRow struct {
		offset uint32
		length uint32
		name   string
	}
	rows := make([]indexRow, 0, count)
	for index := uint32(0); index < count; index++ {
		var raw [264]byte
		if _, err := io.ReadFull(file, raw[:]); err != nil {
			return nil, err
		}
		nameBytes := append([]byte(nil), raw[8:]...)
		for position := range nameBytes {
			nameBytes[position] ^= npkNameKey[position]
		}
		if zero := strings.IndexByte(string(nameBytes), 0); zero >= 0 {
			nameBytes = nameBytes[:zero]
		}
		rows = append(rows, indexRow{
			offset: binary.LittleEndian.Uint32(raw[0:4]),
			length: binary.LittleEndian.Uint32(raw[4:8]),
			name:   strings.TrimSpace(string(nameBytes)),
		})
	}
	members := make([]member, 0, count)
	for _, row := range rows {
		if _, err := file.Seek(int64(row.offset), io.SeekStart); err != nil {
			return nil, err
		}
		data := make([]byte, row.length)
		if _, err := io.ReadFull(file, data); err != nil {
			return nil, err
		}
		members = append(members, member{Name: row.name, Data: data})
	}
	return members, nil
}

func encodeNPK(members []member) ([]byte, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("NPK must contain at least one IMG")
	}
	indexLength := 20 + len(members)*264
	dataOffset := indexLength + sha256.Size
	index := make([]byte, indexLength)
	copy(index[:16], []byte(npkHeader))
	binary.LittleEndian.PutUint32(index[16:20], uint32(len(members)))
	offset := dataOffset
	for position, item := range members {
		if len(item.Data) == 0 || len(item.Name) == 0 || len(item.Name) >= 256 {
			return nil, fmt.Errorf("invalid member name=%q bytes=%d", item.Name, len(item.Data))
		}
		row := index[20+position*264 : 20+(position+1)*264]
		binary.LittleEndian.PutUint32(row[0:4], uint32(offset))
		binary.LittleEndian.PutUint32(row[4:8], uint32(len(item.Data)))
		nameBytes := make([]byte, 256)
		copy(nameBytes, []byte(item.Name))
		for nameIndex := range nameBytes {
			nameBytes[nameIndex] ^= npkNameKey[nameIndex]
		}
		copy(row[8:], nameBytes)
		offset += len(item.Data)
	}
	hashInputLength := len(index) / 17 * 17
	checksum := sha256.Sum256(index[:hashInputLength])
	output := make([]byte, 0, offset)
	output = append(output, index...)
	output = append(output, checksum[:]...)
	for _, item := range members {
		output = append(output, item.Data...)
	}
	return output, nil
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/"))
}
