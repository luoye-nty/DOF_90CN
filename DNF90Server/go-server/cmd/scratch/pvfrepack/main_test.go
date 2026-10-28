package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestValidateRawReplacementRejectsDecodedScript(t *testing.T) {
	original := make([]byte, 10)
	original[0] = 3
	binary.LittleEndian.PutUint32(original[1:5], 1234)
	original[5] = 0
	binary.LittleEndian.PutUint32(original[6:10], 2)
	modified := append([]byte(nil), original...)
	binary.LittleEndian.PutUint32(modified[6:10], 3)
	file := platformpvf.File{DataType: 1}
	if err := validateRawReplacement(file, original, modified); err != nil {
		t.Fatalf("valid token stream rejected: %v", err)
	}
	if err := validateRawReplacement(file, original, []byte("[PHASE MAX]\n3")); err == nil {
		t.Fatal("decoded script text was accepted as raw token data")
	}
}

func TestResolvePackOptionsRejectsInPlaceOutput(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "Script.pvf")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	modified := filepath.Join(root, "modified")
	if err := os.Mkdir(modified, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePackOptions(packOptions{
		basePath:     base,
		modifiedRoot: modified,
		outputPath:   base,
		maxBytes:     1024,
		maxFileBytes: 1024,
	})
	if err == nil {
		t.Fatal("in-place output was accepted")
	}
}

func TestInteractiveModeCanCancel(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "Script.pvf")
	modified := filepath.Join(root, "modified")
	if err := os.WriteFile(base, []byte("not-opened"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(modified, 0o755); err != nil {
		t.Fatal(err)
	}
	input := base + "\n" + modified + "\n" + filepath.Join(root, "out.pvf") + "\n\n\nNO\n"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runInteractive(bufio.NewReader(bytes.NewBufferString(input)), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("已取消，没有生成 PVF")) {
		t.Fatalf("interactive output=%q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("interactive stderr=%q", stderr.String())
	}
}

func TestCommitCandidatePreservesPreviousOutput(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "out.pvf")
	temporary := filepath.Join(root, "candidate.pvf")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup, err := commitCandidate(temporary, output, true)
	if err != nil {
		t.Fatal(err)
	}
	newData, err := os.ReadFile(output)
	if err != nil || string(newData) != "new" {
		t.Fatalf("new output=%q err=%v", newData, err)
	}
	oldData, err := os.ReadFile(backup)
	if err != nil || string(oldData) != "old" {
		t.Fatalf("backup=%q err=%v", oldData, err)
	}
}
