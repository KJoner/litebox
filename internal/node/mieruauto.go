package node

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/mieru"
)

// Mieru 入口的自动下发(R05)。
//
// 停用、到期、超额、撤权都会改变一个 Mieru 入口上该有的用户集合,而 Mieru 一个入口一个 mita 实例、
// 原来只能手工逐个下发 —— 一台只有 Mieru 入口的机器上,被停用的人的凭据会一直留在节点上。
// 这里只做「只动了用户」的那一种:走 mita reload,一条连接都不断(**已建立的会话可能继续存活**,
// 那是 reload 的行为,界面上照实说)。端口、传输层、MTU、出口也有改动的入口一律不碰 —— 那些改动
// 要管理员自己挑时机下发,自动下发顺手把它们一起生效,就是在他没准备好时改了别的东西。

// mieruIdleUserName 是没有任何真实用户时占位用户的名字。它不是 user_xxxxxx 的形状,
// 流量同步按用户代码入账时匹配不到任何人(与链路凭据 chain_xxxxxx 同一个道理)。
const mieruIdleUserName = "litebox_idle"

// mieruIdleUser 是一个入口上谁都没拿到过口令的占位用户:口令由主密钥按入口 id 确定性地派生,
// 同一个入口每次渲染都一样(配置哈希不抖),换一个入口就完全不同。
func (s *Service) mieruIdleUser(inboundID int64) mieru.User {
	raw := s.store.cipher.Derive(fmt.Sprintf("mieru-idle-user/%d", inboundID))
	return mieru.User{Name: mieruIdleUserName, Password: base64.RawURLEncoding.EncodeToString(raw)}
}

// mieruHashes 返回整份配置的哈希与「去掉用户列表之后」的形状哈希。
func mieruHashes(cfg mieru.ServerConfig, raw []byte) (string, string, error) {
	full := sha256.Sum256(raw)
	shape := cfg
	shape.Users = nil
	b, err := json.Marshal(shape)
	if err != nil {
		return "", "", err
	}
	sh := sha256.Sum256(b)
	return hex.EncodeToString(full[:]), hex.EncodeToString(sh[:]), nil
}

// mieruAutoPlan 是自动下发对一个入口的判断。
type mieruAutoPlan int

const (
	mieruAutoSkip    mieruAutoPlan = iota // 不归自动下发管(没上过节点、已停用、已一致)
	mieruAutoReload                       // 只动了用户:reload
	mieruAutoBlocked                      // 还有别的改动没下发:不碰,让管理员手动下发
)

// planMieruAuto 判断一个入口要不要、能不能自动下发。纯函数,由 mieruauto_test.go 盯住。
//
// 形状哈希为空(迁移 0054 之前下发的存量入口)时按 deployed_* 那几列比:它们答不了出口变没变,
// 但出口变更的流程本来就要求管理员手动下发这个入口(MieruChainModal),漏判的代价是这一次 reload
// 没带上出口 —— 与手动下发前的状态相同,不会更坏。
func planMieruAuto(m *MieruInbound, configSHA, shapeSHA string) mieruAutoPlan {
	if !m.Enabled || !m.Deployed() {
		return mieruAutoSkip
	}
	if m.DeployedConfigSHA256 != "" && m.DeployedConfigSHA256 == configSHA {
		return mieruAutoSkip
	}
	sameShape := false
	if m.DeployedShapeSHA256 != "" {
		sameShape = m.DeployedShapeSHA256 == shapeSHA
	} else {
		sameShape = m.DeployedTransport == m.Transport && m.DeployedMultiplexing == m.Multiplexing &&
			m.DeployedMTU == m.MTU && m.DeployedListenPorts() == m.ListenPorts
	}
	if !sameShape {
		return mieruAutoBlocked
	}
	return mieruAutoReload
}

// ErrMieruAutoBlocked 表示有 Mieru 入口的用户变了,但它还有别的改动没下发,自动下发没有碰它。
var ErrMieruAutoBlocked = errors.New("有 Mieru 入口要手动下发")

// DeployMieruChanged 把这台机器上用户集合变了的 Mieru 入口逐个 reload(给部署协调器用)。
//
// 每个入口独立:一个失败不影响另一个,全部做完再汇总报错。没有可做的事时返回一个没有步骤的成功结果。
func (s *Service) DeployMieruChanged(ctx context.Context, nodeID int64) (deployment.Result, error) {
	ctx = context.WithoutCancel(ctx)
	result := deployment.Result{NodeID: nodeID, Kind: deployment.KindMieru, Status: deployment.StatusSuccess}
	list, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return result, err
	}
	var problems []string
	var hardErr error
	for _, m := range list {
		if !m.Enabled || !m.Deployed() {
			continue
		}
		cfg, err := s.DesiredMieruConfig(ctx, m)
		if err != nil {
			problems = append(problems, fmt.Sprintf("入口「%s」的配置渲染不出来:%v", m.DisplayName, err))
			hardErr = err
			continue
		}
		if len(cfg.Users) == 0 {
			cfg.Users = []mieru.User{s.mieruIdleUser(m.ID)}
		}
		raw, err := cfg.MarshalIndent()
		if err != nil {
			return result, err
		}
		configSHA, shapeSHA, err := mieruHashes(cfg, raw)
		if err != nil {
			return result, err
		}
		switch planMieruAuto(m, configSHA, shapeSHA) {
		case mieruAutoSkip:
			continue
		case mieruAutoBlocked:
			problems = append(problems, fmt.Sprintf(
				"入口「%s」的用户变了,但它还有端口 / 传输层 / 出口的改动没下发 —— 自动下发不替你一并生效,"+
					"被移出的人的凭据仍在节点上,去入口列表上手动下发这个 Mieru 入口", m.DisplayName))
			if hardErr == nil {
				hardErr = ErrMieruAutoBlocked
			}
			continue
		}
		r, err := s.DeployMieruWith(ctx, m.ID, true, DeployOptions{autoUsers: true})
		result.Steps = append(result.Steps, r.Steps...)
		if err != nil {
			problems = append(problems, fmt.Sprintf("入口「%s」reload 失败:%v", m.DisplayName, firstLine(err.Error(), 200)))
			hardErr = err
			if r.RollbackResult != "" {
				result.RollbackResult = r.RollbackResult
			}
		}
	}
	if len(problems) > 0 {
		result.Status = deployment.StatusFailed
		result.ErrorMessage = strings.Join(problems, ";")
		if errors.Is(hardErr, ErrMieruAutoBlocked) && len(problems) == 1 {
			return result, fmt.Errorf("%w:%s", ErrMieruAutoBlocked, result.ErrorMessage)
		}
		return result, fmt.Errorf("%s: %w", result.ErrorMessage, hardErr)
	}
	return result, nil
}
