"""CubeProbe 注入前静态预检：确认 DNF.exe 里三个目标的函数头字节与探针内置的一致。

探针在安装时会自己复核一遍（不一致就拒绝打补丁），这个脚本是让"人"也能在
注入之前先看一眼，避免白跑一轮现场。

用法：
    python preflight_check.py
"""
import struct
import sys

EXE = r"D:\test\地下城与勇士\DNF.exe"

# 与 CubeProbe.cpp 里的常量一一对应
TARGETS = [
    ("C2S 编码 sub_33AAFE0", 0x02FAAFE0, bytes([0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x10])),
    ("S2C 解码 sub_33AB0A0", 0x02FAB0A0, bytes([0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x10])),
    ("分发查表 sub_2262430", 0x01E62430, bytes([0x55, 0x8B, 0xEC, 0x83, 0x79, 0x08, 0x00])),
]


def load(path):
    with open(path, "rb") as handle:
        data = handle.read()
    pe = struct.unpack_from("<I", data, 0x3C)[0]
    if data[pe:pe + 4] != b"PE\0\0":
        raise SystemExit("not a PE: %s" % path)
    num_sections = struct.unpack_from("<H", data, pe + 6)[0]
    opt_size = struct.unpack_from("<H", data, pe + 20)[0]
    opt = pe + 24
    # PE32 可选头：ImageBase 在 +28（+24 是 BaseOfData，别搞错）
    image_base = struct.unpack_from("<I", data, opt + 28)[0]
    size_of_image = struct.unpack_from("<I", data, opt + 56)[0]
    dll_chars = struct.unpack_from("<H", data, opt + 70)[0]
    sec = opt + opt_size
    sections = []
    for index in range(num_sections):
        off = sec + index * 40
        name = data[off:off + 8].rstrip(b"\0").decode("latin1")
        vsize, rva, rawsize, rawptr = struct.unpack_from("<IIII", data, off + 8)
        sections.append((name, rva, vsize, rawptr, rawsize))
    return data, image_base, size_of_image, sections


def rva_to_off(sections, rva):
    for name, s_rva, vsize, rawptr, rawsize in sections:
        if s_rva <= rva < s_rva + max(vsize, rawsize):
            delta = rva - s_rva
            if delta < rawsize:
                return rawptr + delta
    return None


def main():
    data, image_base, size_of_image, sections = load(EXE)
    print("DNF.exe        : %s" % EXE)
    print("文件大小       : %d" % len(data))
    print("ImageBase      : 0x%08X" % image_base)
    print("SizeOfImage    : 0x%08X" % size_of_image)
    print()
    bad = 0
    for label, rva, expected in TARGETS:
        off = rva_to_off(sections, rva)
        if off is None:
            print("[FAIL] %-22s RVA 0x%08X 不在任何节里" % (label, rva))
            bad += 1
            continue
        got = data[off:off + len(expected)]
        ok = got == expected
        print("[%s] %-22s RVA 0x%08X 实际=%s 期望=%s"
              % ("OK  " if ok else "FAIL", label, rva,
                 got.hex(" ").upper(), expected.hex(" ").upper()))
        if not ok:
            bad += 1
    print()
    if bad:
        print("结论：%d 个目标对不上 —— 探针注入后会在日志里写 REFUSED 并拒绝打补丁"
              "（不会有副作用，但也取不到证）。先核对客户端版本。" % bad)
        return 1
    print("结论：三个目标全部匹配 —— 探针注入后应当能正常挂钩。")
    print("      运行时日志里每个目标都应出现 INSTALLED；")
    print("      若显示 mode=chain 且下游指向 90CN.dll+0x...，说明 90CN 的钩子被正确串在后面。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
