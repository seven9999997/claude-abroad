package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newClockFixture(t *testing.T, now time.Time) (*Clock, *fakeSys, *httptest.Server) {
	f := newFakeSys(t)
	c := &Clock{Sys: f, Now: func() time.Time { return now }}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return c, f, srv
}

func putClock(t *testing.T, url, body string) (int, map[string]any) {
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

var guestNow = time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)

func TestClockWithinThresholdNoStep(t *testing.T) {
	c, f, srv := newClockFixture(t, guestNow)
	code, out := putClock(t, srv.URL, fmt.Sprintf(`{"unix_ms":%d}`, guestNow.Add(400*time.Millisecond).UnixMilli()))
	if code != 200 || out["stepped"] != false || out["offset_ms"].(float64) != 400 {
		t.Fatalf("偏差在阈值内不应校正：%d %v", code, out)
	}
	if len(f.setTimes) != 0 {
		t.Fatal("不应调用 SetTime")
	}
	st := c.Status()
	if !st.Synced || st.Syncs != 1 || st.Steps != 0 || st.LastOffsetMs != 400 || st.NowUnixMs != guestNow.UnixMilli() {
		t.Fatalf("clock 段：%+v", st)
	}
}

func TestClockStep(t *testing.T) {
	c, f, srv := newClockFixture(t, guestNow)
	host := guestNow.Add(time.Hour)
	code, out := putClock(t, srv.URL, fmt.Sprintf(`{"unix_ms":%d}`, host.UnixMilli()))
	if code != 200 || out["stepped"] != true {
		t.Fatalf("超过阈值应校正：%d %v", code, out)
	}
	if len(f.setTimes) != 1 || !f.setTimes[0].Equal(host) {
		t.Fatalf("SetTime 参数应等于请求时间：%v", f.setTimes)
	}
	if st := c.Status(); st.Steps != 1 || st.LastOffsetMs != 3600000 {
		t.Fatalf("clock 段：%+v", st)
	}
}

func TestClockRejects(t *testing.T) {
	_, f, srv := newClockFixture(t, guestNow)
	for name, body := range map[string]string{
		"太早":      fmt.Sprintf(`{"unix_ms":%d}`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()),
		"太晚":      fmt.Sprintf(`{"unix_ms":%d}`, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()),
		"未知字段":    `{"unix_ms":1763438400000,"x":1}`,
		"缺字段":     `{}`,
		"不是 JSON": `nope`,
		"超长":      `{"unix_ms":1763438400000}` + strings.Repeat(" ", MaxClockBody),
	} {
		if code, _ := putClock(t, srv.URL, body); code != 400 {
			t.Errorf("%s：应回 400，实际 %d", name, code)
		}
	}
	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("GET 应回 405：%d", resp.StatusCode)
	}
	if len(f.setTimes) != 0 {
		t.Fatal("被拒的请求不应调用 SetTime")
	}
}

func TestClockSetTimeFails(t *testing.T) {
	c, f, srv := newClockFixture(t, guestNow)
	f.setTimeErr = errBoom
	code, _ := putClock(t, srv.URL, fmt.Sprintf(`{"unix_ms":%d}`, guestNow.Add(time.Hour).UnixMilli()))
	if code != 500 || c.Status().Synced {
		t.Fatalf("SetTime 失败应回 500 且不算对过时：%d %+v", code, c.Status())
	}
}

func TestHealthClockSection(t *testing.T) {
	st := NewState("k")
	st.SetNet(nil)
	c := &Clock{Sys: newFakeSys(t), Now: func() time.Time { return guestNow }}
	st.SetClock(c.Status)
	h := st.Snapshot()
	if h.Clock == nil || h.Clock.Synced || h.Clock.NowUnixMs != guestNow.UnixMilli() || !h.OK {
		t.Fatalf("health 的 clock 段（未对时也不影响 ok）：%+v", h)
	}
}
