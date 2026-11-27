package pf

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestParseVerboseRules(t *testing.T) {
	out := []byte(`pass out quick on lo0 all no state
  [ Evaluations: 120       Packets: 30        Bytes: 2400        States: 0     ]
@1 pass out quick inet proto tcp all user = 250 flags S/SA keep state
  [ Evaluations: 7         Packets: 0         Bytes: 0           States: 0     ]
block drop out quick all
  [ Evaluations: 9         Packets: 9         Bytes: 540         States: 0     ]
pass out quick on utun5 all no state
`)
	got := ParseVerboseRules(out)
	if len(got) != 4 {
		t.Fatalf("%+v", got)
	}
	if got[0].Packets != 30 || got[0].Evaluations != 120 || got[0].Bytes != 2400 {
		t.Fatalf("第一条：%+v", got[0])
	}
	if got[1].Rule != "pass out quick inet proto tcp all user = 250 flags S/SA keep state" || got[1].Evaluations != 7 {
		t.Fatalf("去掉 @序号：%+v", got[1])
	}
	if got[2].Packets != 9 || got[3].Packets != 0 {
		t.Fatalf("%+v", got)
	}
}

// 只读：只用 -s info、-s rules、-a <锚点> -v -s rules；计数按假 pf 设定的给出。
func TestControllerCounters(t *testing.T) {
	ctx := context.Background()
	f := NewFakePF()
	f.UIDs = map[string]string{"_abroad": "250"}
	c := newCtl(f)
	if _, err := c.Ensure(ctx, mustRender(t, ensureCfg), nil); err != nil {
		t.Fatal(err)
	}
	f.SetRuleCounters(map[int][4]int64{0: {5, 5, 300, 0}})
	f.ResetCalls()
	got, err := c.Counters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Calls(), []string{"info", "mainrules", "anchorcounters"}) {
		t.Fatalf("调用：%v", f.Calls())
	}
	if !got.Enabled || !got.Referenced || len(got.Rules) == 0 || got.Rules[0].Packets != 5 {
		t.Fatalf("%+v", got)
	}
	user := false
	for _, r := range got.Rules {
		user = user || strings.Contains(r.Rule, "user = 250")
	}
	if !user {
		t.Fatalf("应有放行用户那条：%+v", got.Rules)
	}
}
