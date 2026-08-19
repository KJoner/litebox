package subscription

import (
	"context"
	"fmt"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
)

// relaysFor 返回该用户订阅中应当出现的中转线路。
//
// 归属关系走 access.EffectiveRelaysView —— 它已经把"用户在落地上确实有凭据"
// 这一层包含进去了(迁移 0018),这里不再重复判断。重复判断迟早分叉,
// 而分叉的表现是用户在订阅里看得见这条线路、连上去握手直接被拒,
// 还跨了两台机器,排查的人会先去查中转主机。
//
// 附加过滤条件:
//   - 线路未被软删除、未被停用(enabled)、subscription_enabled 为真;
//   - 中转主机未被软删除、未被禁用。
//
// **刻意不要求中转主机"至少成功部署过一次"。** 自建节点那一条要求
// deployed_config_sha256 非空,是因为没部署过的机器上没有用户凭据;
// 而中转主机上根本不存放任何凭据 —— 凭据在落地那边,那一层由视图管着。
// 拿部署状态去卡中转线路,会让一台刚配好、nginx 还没下发的机器
// 连订阅条目都生成不出来,而管理员看不出为什么。
//
// 落地是外部代理时的到期与禁用由 user_effective_external_proxies 管,
// 与直连的外部代理条目完全一致。
func (s *Service) relaysFor(ctx context.Context, userID int64) ([]PhysicalRelay, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.display_name,
		       a.host, a.ipv6_address,
		       CASE WHEN r.public_port = 0 THEN r.listen_port ELSE r.public_port END,
		       a.ipv6_proxy_port,
		       r.target_kind,
		       COALESCE(b.deployed_protocol, ''), COALESCE(b.deployed_ss_method, ''),
		       COALESCE(b.deployed_tcp_fast_open, 0),
		       COALESCE(b.ss_password_encrypted, ''),
		       COALESCE(b.reality_dest, ''), COALESCE(b.reality_pubkey, ''),
		       COALESCE(b.reality_short_id, ''),
		       COALESCE(p.protocol, ''), COALESCE(p.params_encrypted, ''),
		       COALESCE(p.raw_uri_encrypted, '')
		  FROM node_relays r
		  JOIN `+access.EffectiveRelaysView+` er ON er.relay_id = r.id
		  JOIN nodes a ON a.id = r.node_id
		  LEFT JOIN nodes b            ON b.id = r.target_node_id
		  LEFT JOIN external_proxies p ON p.id = r.target_external_id
		 WHERE er.proxy_user_id = ?
		   AND r.deleted_at IS NULL
		   AND r.enabled = 1
		   AND r.subscription_enabled = 1
		   AND a.deleted_at IS NULL
		   AND a.status != 'DISABLED'
		 ORDER BY r.sort_order, r.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	relays := make([]PhysicalRelay, 0)
	for rows.Next() {
		var (
			p                                  PhysicalRelay
			kind                               string
			protocol, ssMethod, ssKeyEnc       string
			tfo                                bool
			realityDest, realityPub, realitySI string
			extProtocol, extParamsEnc, extURI  string
		)
		if err := rows.Scan(&p.DisplayName, &p.Host, &p.IPv6Address, &p.Port, &p.IPv6Port,
			&kind, &protocol, &ssMethod, &tfo, &ssKeyEnc,
			&realityDest, &realityPub, &realitySI,
			&extProtocol, &extParamsEnc, &extURI); err != nil {
			return nil, err
		}

		switch kind {
		case "NODE":
			landing := &RelayNodeLanding{
				TCPFastOpen:      tfo,
				RealityDest:      realityDest,
				RealityPublicKey: realityPub,
				RealityShortID:   realitySI,
			}
			// 解析失败回落到 VLESS:这一列只由 MarkDeployed 写入,
			// 出现未知值说明库被人手工改过。回落而不是报错,
			// 是因为报错会让整份订阅失败,把用户客户端里的节点全部清空。
			landing.Protocol, _ = singbox.ParseProtocol(protocol)
			landing.SSMethod = singbox.SSMethod(ssMethod)
			if landing.Protocol == singbox.ProtocolShadowsocks && ssKeyEnc != "" {
				if landing.SSServerKey, err = s.cipher.Decrypt(ssKeyEnc); err != nil {
					return nil, fmt.Errorf("解密中转线路 %s 落地的 Shadowsocks 密钥: %w",
						p.DisplayName, err)
				}
			}
			p.Node = landing

		case "EXTERNAL":
			landing := &RelayExternalLanding{Protocol: externalproxy.Protocol(extProtocol)}
			if extParamsEnc != "" {
				raw, err := s.cipher.Decrypt(extParamsEnc)
				if err != nil {
					return nil, fmt.Errorf("解密中转线路 %s 落地的参数: %w", p.DisplayName, err)
				}
				if landing.Params, err = externalproxy.ParseParams(raw); err != nil {
					return nil, fmt.Errorf("中转线路 %s: %w", p.DisplayName, err)
				}
			}
			if extURI != "" {
				if landing.RawURI, err = s.cipher.Decrypt(extURI); err != nil {
					return nil, fmt.Errorf("解密中转线路 %s 落地的分享链接: %w", p.DisplayName, err)
				}
			}
			p.External = landing

		default:
			// 落地种类认不出来时跳过这一条,不让整份订阅失败。
			s.logger.Error("中转线路的落地种类无法识别,已跳过",
				"relay", p.DisplayName, "target_kind", kind)
			continue
		}
		relays = append(relays, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ExpandAllRelays(relays), nil
}

// relayEntries 把中转线路转成订阅条目。
//
// 与 entriesFor 同样的容错:单条失败时跳过并记日志,不让整份订阅失败 ——
// 订阅失败会让客户端把已有节点全部清空,而问题可能只出在一条刚加进来的线路上。
func (s *Service) relayEntries(cred Credentials, relays []PhysicalRelay) []Entry {
	entries := make([]Entry, 0, len(relays))
	for _, r := range relays {
		entry, err := EntryForRelay(cred, r)
		if err != nil {
			s.logger.Error("生成中转订阅条目失败,已跳过该线路",
				"relay", r.DisplayName, "error", err)
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}
