//go:build abroaddev

package pf

import (
	"errors"
	"slices"
	"testing"
)

// M6-11b（开发构建）：按选择文件选；读不出、内容不认识时不加载，带说明。测试段逐字的规则。
func TestChooseReturnBlockDev(t *testing.T) {
	if c := ChooseReturnBlock(SetTest, nil); !c.Load || c.Set != SetTest || !slices.Equal(c.Nets, TestNets) {
		t.Fatalf("测试：%+v", c)
	}
	if c := ChooseReturnBlock(SetReal, nil); !c.Load || c.Set != SetReal || !slices.Equal(c.Nets, AnthropicNets) || c.Note == "" {
		t.Fatalf("真实（要有醒目说明）：%+v", c)
	}
	for _, c := range []ReturnBlockChoice{ChooseReturnBlock("", errors.New("不在")), ChooseReturnBlock("all", nil)} {
		if c.Load || c.Note == "" || c.Config().ReturnBlock != nil {
			t.Fatalf("读不出或不认识时不应加载：%+v", c)
		}
	}
	got, err := Render(ChooseReturnBlock(SetTest, nil).Config())
	want := "block return out quick inet to 192.0.2.0/24\nblock return out quick inet6 to 2001:db8:abd::/48\n"
	if err != nil || string(got) != want {
		t.Fatalf("测试段的规则：%v\n%s", err, got)
	}
	if !DevBuild {
		t.Fatal("开发构建 DevBuild 应为真")
	}
}
