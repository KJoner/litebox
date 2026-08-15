package singbox

import (
	"encoding/json"
	"strings"
	"testing"
)

// v3VLESSConfig 是 V4 之前那版渲染器对下面这组参数产出的完整配置,
// 按当时的结构体定义逐字段手写:
//
//	Inbound{Type,Tag,Listen,ListenPort,Users []VLESSUser,TLS InboundTLS}
//	VLESSUser{Name,UUID,Flow}
//
// 这份文本是升级兼容的锚点。V4 把 Inbound 泛化成两种协议共用,
// TLS 变成指针 + omitempty、UUID/Flow 加了 omitempty —— 任何一处
// 影响到 VLESS 的输出,存量节点在升级后的第一次配置比对里就会出现差异,
// 进而被判成"需要部署",十几台机器同时排队重启一遍。
//
// 那种重启不带来任何配置变化,却会踢掉全部在线连接,而管理员看到的
// 只是"升级完所有节点都要重新部署",没有任何线索指向序列化的字段顺序。
const v3VLESSConfig = `{
  "log": {
    "level": "info",
    "timestamp": true
  },
  "inbounds": [
    {
      "type": "vless",
      "tag": "vless-in",
      "listen": "::",
      "listen_port": 443,
      "users": [
        {
          "name": "user_000001",
          "uuid": "0e53ec27-4f42-48da-a473-6ada91959d35",
          "flow": "xtls-rprx-vision"
        },
        {
          "name": "user_000002",
          "uuid": "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
          "flow": "xtls-rprx-vision"
        }
      ],
      "tls": {
        "enabled": true,
        "server_name": "www.fastly.com",
        "reality": {
          "enabled": true,
          "handshake": {
            "server": "www.fastly.com",
            "server_port": 443
          },
          "private_key": "SDgvSbnZoQMzMs1zLNaVpQ0OoI1U-JCsMbUBQvJHR3M",
          "short_id": [
            "dc329d8c"
          ]
        }
      }
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ],
  "experimental": {
    "v2ray_api": {
      "listen": "127.0.0.1:28080",
      "stats": {
        "enabled": true,
        "inbounds": [
          "vless-in"
        ],
        "users": [
          "user_000001",
          "user_000002"
        ]
      }
    }
  }
}
`

func v3Params() NodeParams {
	return NodeParams{
		// 刻意留空:存量节点的 protocol 列由迁移填成 VLESS_REALITY,
		// 但从数据库读出来之前的零值也必须走同一条渲染路径。
		Protocol:          "",
		ListenPort:        443,
		APIPort:           28080,
		RealityDest:       "www.fastly.com",
		RealityPort:       443,
		RealityPrivateKey: "SDgvSbnZoQMzMs1zLNaVpQ0OoI1U-JCsMbUBQvJHR3M",
		ShortID:           "dc329d8c",
		Users: []User{
			{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
			{Code: "user_000002", UUID: "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"},
		},
	}
}

// 存量 VLESS 节点升级后渲染出的配置必须与升级前【逐字节相同】。
func TestVLESSRenderIsByteIdenticalToV3(t *testing.T) {
	got, err := RenderJSON(v3Params())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.JSON) != v3VLESSConfig {
		t.Errorf("VLESS 渲染结果与 V4 之前不一致 —— 升级后全部存量节点会被判成需要部署。\n"+
			"实际:\n%s\n期望:\n%s", got.JSON, v3VLESSConfig)
	}
}

// 显式协议与留空必须产出同一份配置。留空是迁移前读到的零值,
// 两者分叉的话,同一个节点在升级前后会算出两个不同的配置哈希。
func TestExplicitVLESSMatchesEmptyProtocol(t *testing.T) {
	blank, err := RenderJSON(v3Params())
	if err != nil {
		t.Fatal(err)
	}
	p := v3Params()
	p.Protocol = ProtocolVLESSReality
	explicit, err := RenderJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	if blank.SHA256 != explicit.SHA256 {
		t.Errorf("协议留空与显式 VLESS_REALITY 渲染出不同的配置\n留空 %s\n显式 %s",
			blank.SHA256, explicit.SHA256)
	}
}

// Parse 必须能读回 V3 时期节点上的配置文本。
//
// 它是配置比对的入口:读不回来的话,每台存量节点的「比对配置」都会报错,
// 而那是管理员判断"节点上跑的是不是库里这一份"的唯一手段。
func TestParseReadsV3Config(t *testing.T) {
	cfg, err := Parse([]byte(v3VLESSConfig))
	if err != nil {
		t.Fatalf("读不回 V3 配置: %v", err)
	}
	if len(cfg.Inbounds) != 1 {
		t.Fatalf("入站数 = %d", len(cfg.Inbounds))
	}
	in := cfg.Inbounds[0]
	if in.Type != "vless" || in.Tag != "vless-in" {
		t.Errorf("入站 type/tag = %q/%q", in.Type, in.Tag)
	}
	// TLS 从值类型改成了指针。读回旧文本时必须被分配出来,
	// 否则 compareNodeAttrs 会把每台存量节点都当成"没有 TLS 块"。
	if in.TLS == nil {
		t.Fatal("TLS 块没有被解析出来 —— 配置比对会漏报 REALITY 相关的全部差异")
	}
	if in.TLS.Reality.PrivateKey == "" || in.TLS.ServerName != "www.fastly.com" {
		t.Errorf("REALITY 字段解析不全:%+v", in.TLS)
	}
	if len(in.Users) != 2 || in.Users[0].UUID == "" || in.Users[0].Flow != FlowVision {
		t.Errorf("用户解析不全:%+v", in.Users)
	}
	// 读回来的配置与重新渲染的必须一致,否则比对会永远显示有差异。
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Errorf("读回的 V3 配置过不了一致性断言: %v", err)
	}
}

// Shadowsocks 的配置里不能出现 tls 字段。
//
// sing-box 对无关字段是宽容的,不会报错 —— 正因为不报错,一个
// shadowsocks 入站里挂着 "tls": {"enabled": false} 的空壳,
// 会让排查的人先怀疑配置串了协议。
func TestShadowsocksRenderHasNoTLSBlock(t *testing.T) {
	rendered, err := RenderJSON(ssParams())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered.JSON), `"tls"`) {
		t.Errorf("Shadowsocks 配置里出现了 tls 字段:\n%s", rendered.JSON)
	}
	if strings.Contains(string(rendered.JSON), `"uuid"`) ||
		strings.Contains(string(rendered.JSON), `"flow"`) {
		t.Errorf("Shadowsocks 配置里出现了 VLESS 字段:\n%s", rendered.JSON)
	}

	var raw map[string]any
	if err := json.Unmarshal(rendered.JSON, &raw); err != nil {
		t.Fatalf("产出的不是合法 JSON: %v", err)
	}
	in := raw["inbounds"].([]any)[0].(map[string]any)
	if in["type"] != "shadowsocks" || in["tag"] != ShadowsocksInboundTag {
		t.Errorf("入站 type/tag = %v/%v", in["type"], in["tag"])
	}
	if in["method"] != string(SSMethodAES128GCM) {
		t.Errorf("method = %v", in["method"])
	}
}
