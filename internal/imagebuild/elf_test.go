package imagebuild

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"strings"
	"testing"
)

// fakeELF 手工拼一个最小的 ELF64 小端程序：文件头 + 一个 PT_LOAD（可选再加 PT_INTERP，指向 glibc 的链接器）。
func fakeELF(machine elf.Machine, interp bool) []byte {
	if interp {
		return fakeELFInterp(machine, "/lib/ld-linux-aarch64.so.1")
	}
	return fakeELFInterp(machine, "")
}

// fakeELFInterp 同上，interp 非空时加 PT_INTERP。
func fakeELFInterp(machine elf.Machine, interpPath string) []byte {
	var b bytes.Buffer
	interp := interpPath != ""
	nph := 1
	if interp {
		nph = 2
	}
	le := binary.LittleEndian
	h := elf.Header64{
		Ident:     [16]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(machine),
		Version:   uint32(elf.EV_CURRENT),
		Entry:     0x400000,
		Phoff:     64,
		Ehsize:    64,
		Phentsize: 56,
		Phnum:     uint16(nph),
	}
	_ = binary.Write(&b, le, h)
	_ = binary.Write(&b, le, elf.Prog64{Type: uint32(elf.PT_LOAD), Flags: uint32(elf.PF_R | elf.PF_X), Vaddr: 0x400000, Filesz: 0x100, Memsz: 0x100, Align: 0x1000})
	if interp {
		n := uint64(len(interpPath) + 1)
		_ = binary.Write(&b, le, elf.Prog64{Type: uint32(elf.PT_INTERP), Flags: uint32(elf.PF_R), Off: 0xb0, Filesz: n, Memsz: n, Align: 1})
	}
	pad := make([]byte, 0x100)
	copy(pad, interpPath) // 文件头 64 + 两个程序头 112 = 0xb0，解释器字符串紧随其后
	b.Write(pad)
	return b.Bytes()
}

func TestCheckAgentELF(t *testing.T) {
	if err := CheckAgentELF(fakeELF(elf.EM_AARCH64, false)); err != nil {
		t.Fatalf("静态 arm64 程序应通过：%v", err)
	}
	for name, b := range map[string][]byte{
		"x86-64":  fakeELF(elf.EM_X86_64, false),
		"动态链接":    fakeELF(elf.EM_AARCH64, true),
		"不是 ELF":  []byte("#!/bin/sh\n"),
		"截断的 ELF": fakeELF(elf.EM_AARCH64, false)[:20],
	} {
		if err := CheckAgentELF(b); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func TestBuildRejectsBadAgent(t *testing.T) {
	o, _ := setupBuild(t)
	o.Agent = fakeELF(elf.EM_X86_64, false)
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("x86-64 的 agent 应在构建开始时被拒：%v", err)
	}
}

func TestVerifyAgentELF(t *testing.T) {
	o, _ := setupBuild(t)
	o.Agent = fakeELF(elf.EM_AARCH64, false)
	_, err := Build(context.Background(), o)
	must(t, err)
	clean := t.TempDir()
	copyDir(t, o.OutDir, clean)

	cases := map[string]struct {
		init []byte
		item string
		want string
	}{
		"x86-64": {fakeELF(elf.EM_X86_64, false), OutInitramfs, "linux/arm64"},
		"非 ELF":  {[]byte("#!/bin/sh\n"), OutInitramfs, "不是 ELF"},
		"带解释器":   {fakeELF(elf.EM_AARCH64, true), OutInitramfs, "动态链接"},
		"两份不一致":  {append(fakeELF(elf.EM_AARCH64, false), 0), OutRootFS, "不是同一个程序"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		copyDir(t, clean, dir)
		rewriteInitramfs(t, dir+"/"+OutInitramfs, func(tr *Tree) {
			tr.Remove("/init")
			must(t, tr.AddFile("/init", 0o755, c.init))
		})
		found := false
		probs := Verify(dir, Squashfs{})
		for _, p := range probs {
			if p.Item == c.item && strings.Contains(p.Detail, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s：verify 应在 %s 指出“%s”，实际：%v", name, c.item, c.want, probs)
		}
	}
}

func TestCheckRunnableOn(t *testing.T) {
	tr := NewTree()
	must(t, tr.AddFile("/lib/ld-musl-aarch64.so.1", 0o755, []byte("musl")))
	must(t, tr.AddSymlink("/lib/libc.musl-aarch64.so.1", "ld-musl-aarch64.so.1"))
	if err := CheckRunnableOn(tr, fakeELF(elf.EM_AARCH64, false)); err != nil {
		t.Errorf("静态程序应通过：%v", err)
	}
	if err := CheckRunnableOn(tr, fakeELFInterp(elf.EM_AARCH64, "/lib/ld-musl-aarch64.so.1")); err != nil {
		t.Errorf("musl 程序应通过：%v", err)
	}
	if err := CheckRunnableOn(tr, fakeELF(elf.EM_AARCH64, true)); err == nil || !strings.Contains(err.Error(), "ld-linux-aarch64") {
		t.Errorf("glibc 程序应报缺少动态链接器：%v", err)
	}
}
