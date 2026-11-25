//go:build abroaddev

package main

import (
	"bytes"
	"testing"
)

// 开发构建：--outbound-open、--dev-agent 都在；开放模式与 --first-hop 互斥（M6-1）。
func TestDevFlagsInDevBuild(t *testing.T) {
	var w bytes.Buffer
	o, err := parseFlags([]string{"--state-dir", "s", "--outbound-open", "--dev-agent"}, &w)
	if err != nil || !o.outboundOpen || !o.devAgent {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := parseFlags([]string{"--state-dir", "s", "--outbound-open", "--first-hop", "8.8.4.4:443"}, &w); err == nil {
		t.Error("开放模式和 --first-hop 应互斥")
	}
}
