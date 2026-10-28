package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const scriptTokenSize = 5

func main() {
	root := flag.String("dir", "", "raw PVF replacement directory")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -dir is required")
		os.Exit(2)
	}
	for _, rel := range []string{
		"stackable/ptn_hplow.stk",
		"stackable/ptn_mplow.stk",
	} {
		if err := patchPotion(filepath.Join(*root, filepath.FromSlash(rel))); err != nil {
			fmt.Fprintf(os.Stderr, "FAILED: %s: %v\n", rel, err)
			os.Exit(1)
		}
		fmt.Println("patched", rel)
	}
}

func patchPotion(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) != 45*scriptTokenSize {
		return fmt.Errorf("unexpected raw script size %d", len(raw))
	}
	for index, wantType := range map[int]byte{
		15: 3, 16: 0, // [minimum level] 6
		21: 3, 22: 0, // [cool time] 10000
		29: 3, 30: 6, 31: 0, // [stackable type] `[waste]` 4
		38: 3, 39: 0, // [cooltime group] N
	} {
		if got := raw[index*scriptTokenSize]; got != wantType {
			return fmt.Errorf("token %d type=%d want=%d", index, got, wantType)
		}
	}
	if got := tokenInt(raw, 16); got != 6 {
		return fmt.Errorf("minimum level=%d want=6", got)
	}
	if got := tokenInt(raw, 22); got != 10000 {
		return fmt.Errorf("cool time=%d want=10000", got)
	}
	if got := tokenInt(raw, 31); got != 4 {
		return fmt.Errorf("waste subtype=%d want=4", got)
	}

	// Make the two beginner recovery items available from level 1 and route
	// them through the generic [waste] client path. The subtype-4 recovery
	// path suppresses op44 locally when the matching resource is already full.
	// Removing both cooldown tokens also removes the native 10-second local
	// and shared-group gate; the server cannot lift either gate because no
	// packet is sent while the client blocks the action.
	putTokenInt(raw, 16, 1)
	putTokenInt(raw, 31, 0)
	out := make([]byte, 0, len(raw)-4*scriptTokenSize)
	for index := 0; index < len(raw)/scriptTokenSize; index++ {
		if index == 21 || index == 22 || index == 38 || index == 39 {
			continue
		}
		start := index * scriptTokenSize
		out = append(out, raw[start:start+scriptTokenSize]...)
	}
	if len(out) != 41*scriptTokenSize {
		return fmt.Errorf("patched script size=%d want=%d", len(out), 41*scriptTokenSize)
	}
	return os.WriteFile(path, out, 0o644)
}

func tokenInt(raw []byte, index int) int32 {
	start := index*scriptTokenSize + 1
	return int32(binary.LittleEndian.Uint32(raw[start : start+4]))
}

func putTokenInt(raw []byte, index int, value int32) {
	start := index*scriptTokenSize + 1
	binary.LittleEndian.PutUint32(raw[start:start+4], uint32(value))
}
