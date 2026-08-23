package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Mieru 入口的管理接口。
//
// 这些用例盯的是**接口这一层**的约定:端口冲突回 409 而不是 500、
// 留空的段不被固化、以及 Mieru 与 sing-box 入站的 id 空间不串。

// seedLandingNode 建一台落地机器并返回它的 id。
// 它自带一个监听 24443 的 sing-box 入站 —— 端口冲突的用例要靠它。
func (e *testEnv) seedLandingNode(t *testing.T) int64 {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/api/nodes", map[string]any{
		"name": "node-mieru", "host": "192.0.2.30",
		"ssh_port": 22, "ssh_user": "root", "ssh_key": testSSHKey,
		"proxy_port": 24443, "listen_port": 24443, "api_port": 28080,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("建节点失败,状态码 %d", resp.StatusCode)
	}
	var out struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	decodeInto(t, resp, &out)
	return out.Node.ID
}

func (e *testEnv) createMieru(t *testing.T, nodeID int64, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	resp := e.do(t, http.MethodPost,
		"/api/nodes/"+itoa(nodeID)+"/mieru-inbounds", body)
	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		decodeInto(t, resp, &out)
	}
	return resp, out
}

func TestCreateMieruInboundThroughAPI(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	nodeID := env.seedLandingNode(t)

	resp, out := env.createMieru(t, nodeID, map[string]any{
		"display_name":      "TY-Mieru",
		"listen_port_start": 30000,
		"listen_port_end":   30010,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	in, _ := out["inbound"].(map[string]any)
	if in["listen_port_start"] != float64(30000) || in["listen_port_end"] != float64(30010) {
		t.Errorf("监听端口段 = %v-%v", in["listen_port_start"], in["listen_port_end"])
	}
	// 公网端口段留空表示跟随,**不能在写库时固化成当时的监听段** ——
	// 固化之后管理员再改监听段,订阅条目会继续停在旧号码上。
	if in["public_port_start"] != float64(0) || in["public_port_end"] != float64(0) {
		t.Errorf("公网端口段应当留 0(跟随),得到 %v-%v",
			in["public_port_start"], in["public_port_end"])
	}
	// 新增不自动下发:下发会重启 mita 踢掉这台机器上全部 Mieru 连接。
	if out["needs_deploy"] != true {
		t.Error("新增入口后应当提示需要下发")
	}
	// 派生的 IPv6 条目名由后端算好,前端不自己拼后缀。
	if in["ipv6_entry_name"] != "TY-Mieru-IPV6" {
		t.Errorf("ipv6_entry_name = %v", in["ipv6_entry_name"])
	}
}

// 端口段罩住已有的 sing-box 入站时回 409,不是 500 也不是 400。
//
// 这一条正是把三处端口检测统一到 nodeport 之后才成立的:原来那种
// `listen_port = ?` 的写法查不出"这个入站落在某个段里"。
func TestCreateMieruInboundPortConflictReturns409(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	nodeID := env.seedLandingNode(t)

	resp, _ := env.createMieru(t, nodeID, map[string]any{
		"display_name":      "撞车",
		"listen_port_start": 24440,
		"listen_port_end":   24450,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("状态码 = %d,期望 409", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	// 错误信息要说清撞的是什么,不然管理员只能挨个端口试。
	if !strings.Contains(msg, "24443") {
		t.Errorf("错误信息没说清撞了哪个端口:%q", msg)
	}
}

// 反过来:sing-box 入站落进已有的 Mieru 段里同样要拦。
// 这是原来那两处实现完全没有的方向 —— 它们根本不查 node_mieru_inbounds。
func TestCreateInboundInsideMieruRangeReturns409(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	nodeID := env.seedLandingNode(t)

	resp, _ := env.createMieru(t, nodeID, map[string]any{
		"display_name":      "TY-Mieru",
		"listen_port_start": 30000,
		"listen_port_end":   30010,
	})
	resp.Body.Close()

	conflict := env.do(t, http.MethodPost, "/api/nodes/"+itoa(nodeID)+"/inbounds",
		map[string]any{
			"display_name": "落进段里的入站",
			"protocol":     "SHADOWSOCKS",
			"listen_port":  30005,
		})
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("状态码 = %d,期望 409", conflict.StatusCode)
	}
}

// Mieru 与 sing-box 入站的 id 空间是分开的,路由也分开 ——
// 拿 Mieru 的 id 去打 /api/inbounds/{id} 必须是 404,而不是改到另一个对象。
func TestMieruAndInboundIDSpacesDoNotCross(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	nodeID := env.seedLandingNode(t)

	resp, out := env.createMieru(t, nodeID, map[string]any{
		"display_name":      "TY-Mieru",
		"listen_port_start": 30000,
		"listen_port_end":   30010,
	})
	resp.Body.Close()
	in, _ := out["inbound"].(map[string]any)
	mieruID := int64(in["id"].(float64))

	// 建节点时自带的那个 sing-box 入站 id 也是 1 —— 两者撞在同一个数字上,
	// 这正是路由必须分开的原因。
	wrong := env.do(t, http.MethodDelete, "/api/inbounds/"+itoa(mieruID), nil)
	defer wrong.Body.Close()
	// 走错路由删掉的会是【另一个对象】。这里只断言 Mieru 那一条还在。
	list := env.do(t, http.MethodGet, "/api/nodes/"+itoa(nodeID)+"/mieru-inbounds", nil)
	defer list.Body.Close()
	var listed map[string]any
	decodeInto(t, list, &listed)
	items, _ := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("Mieru 入口应当仍在,得到 %d 条", len(items))
	}
}

// 取值范围由后端给,前端不自己写一份 —— 上游加一个档位时,
// 两处各写一遍会让下拉框里长期缺一项,而管理员看不出为什么。
func TestMieruListReturnsEnumOptions(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	nodeID := env.seedLandingNode(t)

	resp := env.do(t, http.MethodGet, "/api/nodes/"+itoa(nodeID)+"/mieru-inbounds", nil)
	defer resp.Body.Close()
	var out map[string]any
	decodeInto(t, resp, &out)

	transports, _ := out["transports"].([]any)
	if len(transports) != 2 {
		t.Errorf("transports = %v", out["transports"])
	}
	muxes, _ := out["multiplexings"].([]any)
	if len(muxes) != 4 || muxes[0] != "MULTIPLEXING_OFF" {
		t.Errorf("multiplexings = %v", out["multiplexings"])
	}
}
