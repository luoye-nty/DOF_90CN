package main

import (
	"flag"
	"reflect"
	"testing"
)

// grepFlags 复刻 runGrep 的选项表，用来验证"模式在前、选项在后"也能被正确解析。
func grepFlags() (fs *flag.FlagSet, dir, keyFile *string, ignoreCase, lineNumbers, showFile *bool) {
	fs = newFlagSet("grep-test")
	dir = fs.String("dir", "", "")
	keyFile = fs.String("key", "", "")
	ignoreCase = fs.Bool("i", false, "")
	lineNumbers = fs.Bool("n", false, "")
	showFile = fs.Bool("f", false, "")
	return fs, dir, keyFile, ignoreCase, lineNumbers, showFile
}

// 这是真实踩过的坑：标准库 flag 遇到第一个非选项参数就停止解析，
// 于是文档里的 `logtool grep PATTERN -dir logs -key K` 会让 -dir/-key 变成模式的一部分，
// 悄悄退回默认路径（表现为"读了错误的密钥/目录"，而不是报错）。
func TestGrepAcceptsOptionsAfterPattern(t *testing.T) {
	fs, dir, keyFile, _, lineNumbers, _ := grepFlags()
	err := parseInterspersed(fs, []string{"monster-death-blocked", "-dir", `D:\logs`, "-n", "-key", `D:\k\p.key`}, "dir", "key")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *dir != `D:\logs` {
		t.Errorf("dir = %q, want D:\\logs", *dir)
	}
	if *keyFile != `D:\k\p.key` {
		t.Errorf("key = %q, want D:\\k\\p.key", *keyFile)
	}
	if !*lineNumbers {
		t.Error("-n after the pattern was not honoured")
	}
	if want := []string{"monster-death-blocked"}; !reflect.DeepEqual(fs.Args(), want) {
		t.Errorf("pattern = %v, want %v", fs.Args(), want)
	}
}

func TestGrepAcceptsOptionsBeforePattern(t *testing.T) {
	fs, dir, keyFile, _, _, _ := grepFlags()
	err := parseInterspersed(fs, []string{"-dir", "logs", "-key", "k", "key=name"}, "dir", "key")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *dir != "logs" || *keyFile != "k" {
		t.Fatalf("dir=%q key=%q", *dir, *keyFile)
	}
	if want := []string{"key=name"}; !reflect.DeepEqual(fs.Args(), want) {
		t.Errorf("pattern = %v, want %v", fs.Args(), want)
	}
}

// 模式里含空格时按位置参数原样传下来（runGrep 再拼回去），不能被当成选项。
func TestGrepKeepsMultiTokenPattern(t *testing.T) {
	fs, _, _, _, _, _ := grepFlags()
	if err := parseInterspersed(fs, []string{"-dir", "logs", "owner", "not", "ready"}, "dir", "key"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := []string{"owner", "not", "ready"}; !reflect.DeepEqual(fs.Args(), want) {
		t.Errorf("pattern = %v, want %v", fs.Args(), want)
	}
}

// -- 之后一律是位置参数：想搜 "-dir" 这种以短横线开头的字面量时必须能搜，
// 而且分隔符本身不能被吞掉（否则搬后面的位置参数又会被 flag 包当成选项）。
func TestGrepSeparatorTreatsRestAsPattern(t *testing.T) {
	fs, dir, _, _, _, _ := grepFlags()
	if err := parseInterspersed(fs, []string{"-dir", "logs", "--", "-n", "-dir"}, "dir", "key"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *dir != "logs" {
		t.Errorf("dir = %q, want logs", *dir)
	}
	if want := []string{"-n", "-dir"}; !reflect.DeepEqual(fs.Args(), want) {
		t.Errorf("pattern = %v, want %v", fs.Args(), want)
	}
}

func TestGrepInlineFlagValueStillParsed(t *testing.T) {
	fs, dir, _, _, _, _ := grepFlags()
	if err := parseInterspersed(fs, []string{"needle", "-dir=logs"}, "dir", "key"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *dir != "logs" {
		t.Errorf("dir = %q, want logs", *dir)
	}
}
