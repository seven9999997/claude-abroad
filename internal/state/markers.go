package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"claude-abroad/internal/pf"
)

// 状态目录里的两个标记文件（abroadd 写；guard、应急程序只读，M6-11b）。
const (
	// PreparedFile：首次准备（写入解析锁和托管设置）完成的标记（M6-8a）。
	PreparedFile = "prepared"
	// UninstalledFile：已经一键卸载的标记（M6-5a）。
	UninstalledFile = "uninstalled"
)

// ReturnBlockMarked 报告状态目录里的标记是否要求回国后保留“回国拦截”（M6-11b，设计 10.2 第五道）：首次准备的标记在、
// 卸载标记不在。abroadd 另外还看自己是否打开了首次准备、是否正在卸载；guard 和应急程序只看这两个文件。
func ReturnBlockMarked(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, PreparedFile)); err != nil {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, UninstalledFile))
	return os.IsNotExist(err)
}

// FileExists 报告状态目录里有没有状态文件（没有表示全新安装、abroadd 还没写过）。
func FileExists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, FileName))
	return err == nil
}

// ReturnBlockSetFile 是回国拦截“用哪一套地址段”的选择文件（M6-11b）：abroadd 在首次准备时原子写入（0600），内容只有
// 一个取值（real 或 test）；guard、应急程序、abroadd 都经 ChooseReturnBlock 读它。
const ReturnBlockSetFile = "return-block-set"

// WriteReturnBlockSet 原子写选择文件（同目录临时文件加改名，0600）。
func WriteReturnBlockSet(dir string, set pf.ReturnBlockSet) error {
	f, err := os.CreateTemp(dir, "."+ReturnBlockSetFile+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(string(set) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ReturnBlockSetFile))
}

// ReadReturnBlockSet 读选择文件：不是普通文件、太大、权限对组和其他人开放、内容不认识都返回错误。
func ReadReturnBlockSet(dir string) (pf.ReturnBlockSet, error) {
	path := filepath.Join(dir, ReturnBlockSetFile)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 64 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("回国拦截的选择文件不是普通文件、太大或权限太宽")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	switch s := pf.ReturnBlockSet(strings.TrimSpace(string(b))); s {
	case pf.SetReal, pf.SetTest:
		return s, nil
	}
	return "", errors.New("回国拦截的选择文件内容不认识")
}

// ChooseReturnBlock 按状态目录里的选择文件选回国拦截的地址段（规则见 pf.ChooseReturnBlock）。
func ChooseReturnBlock(dir string) pf.ReturnBlockChoice {
	return pf.ChooseReturnBlock(ReadReturnBlockSet(dir))
}
