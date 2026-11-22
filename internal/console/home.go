package console

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strconv"
)

// 登录用户的主目录（M5-5，m5-plan v2.3 决定 6）：由控制台用户的 uid 查出，供冲突检查读 shell 配置、
// 环境检测第 15、16 项使用。没有人登录时返回 ErrNoConsoleUser，调用方按各自约定跳过或记“无法判断”。

// LookupID 查 uid 对应的用户（可替换，测试用）。
var LookupID = user.LookupId

// HomeDirOf 返回 uid 的主目录（必须是绝对路径）。
func HomeDirOf(uid int, lookup func(string) (*user.User, error)) (string, error) {
	u, err := lookup(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" || u.HomeDir[0] != '/' {
		return "", errors.New("查到的主目录不是绝对路径")
	}
	return u.HomeDir, nil
}

// HomeDir 返回当前控制台用户的主目录。
func HomeDir() (string, error) {
	uid, err := UID()
	if err != nil {
		return "", err
	}
	return HomeDirOf(uid, LookupID)
}

// HomeFS 是“当前控制台用户的主目录”的只读文件系统：每次打开文件时重新取控制台用户（换了人登录也跟着换）；
// 没有人登录或查不到时一律 fs.ErrNotExist（读 shell 配置的一项因此跳过）。
type HomeFS struct {
	// Dir 取主目录（nil 用 HomeDir）。
	Dir func() (string, error)
}

// Open 实现 fs.FS。
func (h HomeFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	dir := h.Dir
	if dir == nil {
		dir = HomeDir
	}
	d, err := dir()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return os.DirFS(d).Open(name)
}
