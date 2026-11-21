package r3

import (
	"context"
	"testing"
	"time"

	"claude-abroad/internal/procinfo"
)

func TestPercentileAndPass(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	if Percentile(ds, 50) != 50*time.Millisecond || Percentile(ds, 99) != 99*time.Millisecond || Percentile(nil, 50) != 0 {
		t.Fatal("分位数不对")
	}
	ok := Report{N: 100, Accepted: 100, Right: 96, P99: 10 * time.Millisecond, MinRate: 95, MaxP99: 20 * time.Millisecond}
	if !ok.Pass() {
		t.Fatal("应达标")
	}
	for _, r := range []Report{
		{N: 100, Accepted: 100, Right: 96, Wrong: 1, P99: time.Millisecond, MinRate: 95},
		{N: 100, Accepted: 100, Right: 94, P99: time.Millisecond, MinRate: 95},
		{N: 100, Accepted: 100, Right: 100, P99: 21 * time.Millisecond, MinRate: 95, MaxP99: 20 * time.Millisecond},
	} {
		if r.Pass() {
			t.Fatalf("不应达标：%+v", r)
		}
	}
}

// self 模式用假的连接表：表里永远找不到，结果全是识别不到，验证统计和收尾不挂住。
func TestRunSelfWithEmptyTable(t *testing.T) {
	id := &procinfo.Identifier{Table: &procinfo.FakeTable{}, Procs: &procinfo.FakeProcs{}, Retries: 1, RetryDelay: time.Millisecond}
	rep, err := Run(context.Background(), Options{Mode: "self", N: 30, Batch: 7, Ident: id, Timeout: 20 * time.Second, MinRate: 95})
	if err != nil || rep.Accepted != 30 || rep.Unknown != 30 || rep.Pass() {
		t.Fatalf("%+v %v", rep, err)
	}
	if rep.SnapshotReads == 0 {
		t.Fatal("读连接表次数应被统计")
	}
}

func TestP99Optional(t *testing.T) {
	r := Report{N: 300, Accepted: 300, Right: 280, P99: time.Second, MinRate: 90}
	if !r.Pass() {
		t.Fatal("MaxP99 为 0 时不看耗时，识别率 93% ≥ 90% 应达标")
	}
}
