package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"claude-abroad/internal/guestabi"
)

// SingBoxSet 是 sing-box 配置的唯一写入者（M4-7）：main 加开发出口加分流出口，任一部分变了就整体重写并重启 sing-box。
// 开发出口和分流出口各自只改自己那部分，不会互相覆盖。
type SingBoxSet struct {
	Path     string  // sing-box 配置文件
	Password string  // socks 入站口令
	Engine   *Engine // sing-box（nil 时只写文件，测试用）
	Base     context.Context
	// WaitFor 是重启后等就绪的上限（默认 15 秒）。
	WaitFor time.Duration

	mu         sync.Mutex
	dev, exits []SingBoxExit
}

// SetDev 替换开发出口。
func (s *SingBoxSet) SetDev(list []SingBoxExit) error {
	return s.set(func() { s.dev = append([]SingBoxExit(nil), list...) }, func(old []SingBoxExit) { s.dev = old }, s.dev)
}

// SetExits 替换分流出口。
func (s *SingBoxSet) SetExits(list []SingBoxExit) error {
	return s.set(func() { s.exits = append([]SingBoxExit(nil), list...) }, func(old []SingBoxExit) { s.exits = old }, s.exits)
}

func (s *SingBoxSet) set(apply func(), undo func([]SingBoxExit), old []SingBoxExit) error {
	s.mu.Lock()
	apply()
	all := append([]SingBoxExit{{Tag: guestabi.ExitMain, Port: guestabi.MainProxyPort}}, s.dev...)
	all = append(all, s.exits...)
	cfg, err := SingBoxConfig(all, s.Password)
	if err != nil {
		undo(old) // 生成不了（如标签冲突）：不改
		s.mu.Unlock()
		return err
	}
	if err := writeAtomic(s.Path, cfg); err != nil {
		undo(old)
		s.mu.Unlock()
		return fmt.Errorf("写 sing-box 配置：%w", err)
	}
	s.mu.Unlock()
	if s.Engine == nil {
		return nil
	}
	d := s.WaitFor
	if d <= 0 {
		d = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(s.Base, d)
	defer cancel()
	gen := s.Engine.Restart(ctx)
	if err := s.Engine.WaitRunning(ctx, gen); err != nil {
		return fmt.Errorf("sing-box 重启后没有就绪：%w", err)
	}
	return nil
}
