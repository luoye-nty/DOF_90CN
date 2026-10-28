// Scratch tool: decode op14 (item list) and op19 (move) packets out of the
// server packet log, so the exact rows the client received can be inspected.
//
//	go run ./cmd/scratch/pktdecode -log <packet_log.txt> -after 2026-09-21T06:14
//
// op14 body: [listType u8][count u16][row x count], row = 119 bytes with
// slot u16 @0x00, itemID u32 @0x02, count u32 @0x06.
// op19 body (28 bytes): srcList u8, srcSlot u16, srcInstance u32, moveCount u32,
// dstList u8, dstSlot u16, dstInstance u32, dstStack u32, actor i32, b0, b1.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
)

const rowWireSize = 119

func main() {
	logPath := flag.String("log", "", "packet log path")
	after := flag.String("after", "", "only lines at/after this time prefix")
	flag.Parse()
	if *logPath == "" {
		fmt.Fprintln(os.Stderr, "missing -log")
		os.Exit(2)
	}
	file, err := os.Open(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if *after != "" {
			if len(line) < len(*after) || line[:len(*after)] < *after {
				continue
			}
		}
		if !strings.Contains(line, " msg_id=14 ") && !strings.Contains(line, " type=14 ") &&
			!strings.Contains(line, "MOVE_ITEMSPACE") {
			continue
		}
		at := ""
		if len(line) > 27 {
			at = line[11:23]
		}
		idx := strings.Index(line, "hex=")
		if idx < 0 {
			continue
		}
		raw, err := hex.DecodeString(strings.TrimSpace(line[idx+4:]))
		if err != nil {
			continue
		}
		kind := "?"
		switch {
		case strings.Contains(line, "MOVE_ITEMSPACE"):
			kind = "op19"
		case strings.Contains(line, "msg_id=14 "):
			kind = "op14"
		}
		fmt.Printf("=== %s %s hex=%d bytes\n", at, kind, len(raw))
		switch kind {
		case "op19":
			decodeMove(raw)
		case "op14":
			decodeList(raw)
		}
	}
}

// decodeMove skips a 13-byte legacy header (RECV) or 16-byte upper header (SEND).
func decodeMove(raw []byte) {
	for _, header := range []int{13, 16} {
		if len(raw) < header+28 {
			continue
		}
		body := raw[header:]
		if len(body) != 28 {
			continue
		}
		srcList := body[0]
		srcSlot := int16(binary.LittleEndian.Uint16(body[1:3]))
		srcInstance := binary.LittleEndian.Uint32(body[3:7])
		moveCount := binary.LittleEndian.Uint32(body[7:11])
		dstList := body[11]
		dstSlot := int16(binary.LittleEndian.Uint16(body[12:14]))
		fmt.Printf("    header=%d src=%d:%d item=%d count=%d dst=%d:%d\n",
			header, srcList, srcSlot, srcInstance, moveCount, dstList, dstSlot)
		return
	}
	// 11-byte success ACK
	if len(raw) >= 16+11 {
		body := raw[16:]
		if len(body) == 11 {
			fmt.Printf("    ack header=16 ok=%d src=%d:%d count=%d dst=%d:%d\n",
				body[0], body[1], int16(binary.LittleEndian.Uint16(body[2:4])),
				binary.LittleEndian.Uint32(body[4:8]), body[8],
				int16(binary.LittleEndian.Uint16(body[9:11])))
			return
		}
	}
	fmt.Printf("    (undecoded %d bytes)\n", len(raw))
}

func decodeList(raw []byte) {
	if len(raw) < 16+3 {
		fmt.Printf("    (short %d)\n", len(raw))
		return
	}
	body := raw[16:]
	listType := body[0]
	count := int(binary.LittleEndian.Uint16(body[1:3]))
	if len(body) != 3+count*rowWireSize {
		fmt.Printf("    list=%d count=%d body=%d (shape mismatch)\n", listType, count, len(body))
	}
	fmt.Printf("    list=%d rows=%d\n", listType, count)
	for index := 0; index < count; index++ {
		start := 3 + index*rowWireSize
		if start+10 > len(body) {
			fmt.Printf("      row %d truncated\n", index)
			return
		}
		row := body[start : start+rowWireSize]
		slot := int16(binary.LittleEndian.Uint16(row[0:2]))
		itemID := binary.LittleEndian.Uint32(row[2:6])
		amount := binary.LittleEndian.Uint32(row[6:10])
		fmt.Printf("      slot=%-4d item=%-10d count=%d\n", slot, itemID, amount)
	}
}
