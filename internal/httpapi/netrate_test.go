package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/litebox/litebox/internal/node"
)

// 卡片网速接口只读内存、不连节点;采样关掉时回答 0 间隔与空数组,页面据此不轮询。
func TestNodeNetRatesEndpoint(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	read := func() (int, []node.NetRate) {
		t.Helper()
		resp := env.do(t, http.MethodGet, "/api/metrics/net-rates", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 %d", resp.StatusCode)
		}
		var body struct {
			Interval int             `json:"interval_seconds"`
			Items    json.RawMessage `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body.Items) == "null" {
			t.Fatal("items 不能是 null")
		}
		var items []node.NetRate
		if err := json.Unmarshal(body.Items, &items); err != nil {
			t.Fatal(err)
		}
		return body.Interval, items
	}

	if iv, items := read(); iv != 0 || len(items) != 0 {
		t.Fatalf("采样关闭时应当是 0 间隔、空数组: %d %v", iv, items)
	}

	env.api.netRates = node.NewNetRateSampler(node.NetRateOptions{
		Service: env.api.nodes,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if iv, items := read(); iv != 30 || len(items) != 0 {
		t.Fatalf("默认 30 秒、还没有读数: %d %v", iv, items)
	}
}
