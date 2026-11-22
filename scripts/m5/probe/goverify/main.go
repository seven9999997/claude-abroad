//go:build darwin

// goverify 是 M5-1 探查 E1 的 Go 一侧（模拟 abroadd）：以 root 运行，不带交互地核验应用交来的系统授权外部形式
// 是否拥有权限项。用一个只包两个系统函数的极小 cgo 文件（只链接系统的 Security.framework，不引入任何第三方依赖）。
// 不用 cgo 的做法（经 purego 动态调用）要新增一个第三方依赖，会话里没有引入，见 docs/m5-probe.md。
//
//	sudo goverify <外部形式的十六进制> [权限项]
package main

/*
#cgo LDFLAGS: -framework Security
#include <Security/Authorization.h>
#include <stdlib.h>
#include <string.h>

static OSStatus verify_right(const unsigned char *ext, const char *right) {
	AuthorizationExternalForm form;
	memcpy(form.bytes, ext, kAuthorizationExternalFormLength);
	AuthorizationRef ref = NULL;
	OSStatus st = AuthorizationCreateFromExternalForm(&form, &ref);
	if (st != errAuthorizationSuccess) return st;
	AuthorizationItem item = { right, 0, NULL, 0 };
	AuthorizationRights rights = { 1, &item };
	st = AuthorizationCopyRights(ref, &rights, NULL, kAuthorizationFlagExtendRights, NULL);
	AuthorizationFree(ref, kAuthorizationFlagDefaults);
	return st;
}
*/
import "C"

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"unsafe"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法：sudo goverify <外部形式的十六进制> [权限项]")
		os.Exit(2)
	}
	ext, err := hex.DecodeString(strings.TrimSpace(os.Args[1]))
	if err != nil || len(ext) != 32 {
		fmt.Fprintln(os.Stderr, "外部形式应是 32 字节（64 个十六进制字符）")
		os.Exit(2)
	}
	right := "com.claude-abroad.sensitive"
	if len(os.Args) >= 3 {
		right = os.Args[2]
	}
	cr := C.CString(right)
	defer C.free(unsafe.Pointer(cr))
	ce := C.CBytes(ext)
	defer C.free(ce)
	st := C.verify_right((*C.uchar)(ce), cr)
	if st == 0 {
		fmt.Printf("Go 核验：通过（%s，以 uid %d 运行）\n", right, os.Getuid())
		return
	}
	fmt.Printf("Go 核验：不通过（%s，OSStatus %d，以 uid %d 运行）\n", right, int(st), os.Getuid())
	os.Exit(3)
}
