//go:build !abroaddev

package pf

import (
	"errors"
	"slices"
	"testing"
)

// M6-11b（发布构建）：选择文件写什么、在不在，都按真实段。
func TestChooseReturnBlockRelease(t *testing.T) {
	for _, c := range []struct {
		set ReturnBlockSet
		err error
	}{{SetReal, nil}, {SetTest, nil}, {"", errors.New("不在")}, {"all", nil}} {
		got := ChooseReturnBlock(c.set, c.err)
		if !got.Load || got.Set != SetReal || !slices.Equal(got.Nets, AnthropicNets) {
			t.Fatalf("%q %v：%+v", c.set, c.err, got)
		}
	}
	if DevBuild {
		t.Fatal("发布构建 DevBuild 应为假")
	}
}
