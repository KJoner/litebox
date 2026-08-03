package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/user"
)

// AlertLevel 是预警级别。
type AlertLevel string

const (
	AlertWarning AlertLevel = "warning"
	AlertError   AlertLevel = "error"
)

// Alert 是一条管理员预警。
type Alert struct {
	Level AlertLevel `json:"level"`
	// Category 用于前端分组与跳转:user / node。
	Category string `json:"category"`
	Target   string `json:"target"`
	// TargetID 供前端跳转到对应详情。
	TargetID int64  `json:"target_id"`
	Message  string `json:"message"`
}

// metricsStaleAfter 是监控数据的过期阈值,取采集周期(5 分钟)的两倍。
// 只错过一次采集很常见(节点忙、网络抖),按一倍算会天天误报。
const metricsStaleAfter = 10 * time.Minute

// buildDashboardAlerts 是纯函数,便于直接对边界情况写测试。
//
// 刻意不把"监控数据过期"算成节点离线:采集是走 SSH 的独立通道,
// 它失败不代表 sing-box 停了。把两件事混在一起会让管理员在代理完全正常时
// 收到"节点离线",几次之后就再也不看这个列表了。
func buildDashboardAlerts(users []*user.User, nodes []*node.Node,
	metrics map[int64]node.Metrics, now time.Time) []Alert {
	alerts := make([]Alert, 0)

	for _, u := range users {
		if u.Status == user.StatusDisabled {
			continue
		}
		if u.QuotaBytes > 0 {
			percent := float64(u.UsedTotal()) / float64(u.QuotaBytes) * 100
			switch {
			case percent >= 100:
				alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID,
					"流量已用完"})
			case percent >= 95:
				alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID,
					fmt.Sprintf("流量已用 %.0f%%", percent)})
			case percent >= 80:
				alerts = append(alerts, Alert{AlertWarning, "user", u.DisplayName, u.ID,
					fmt.Sprintf("流量已用 %.0f%%", percent)})
			}
		}
		if u.ExpiresAt != nil && *u.ExpiresAt != "" {
			if exp, err := time.Parse(time.RFC3339, *u.ExpiresAt); err == nil {
				days := int(exp.Sub(now).Hours() / 24)
				switch {
				case !exp.After(now):
					alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID, "已到期"})
				case days <= 3:
					alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID,
						fmt.Sprintf("%d 天内到期", days+1)})
				case days <= 7:
					alerts = append(alerts, Alert{AlertWarning, "user", u.DisplayName, u.ID,
						fmt.Sprintf("%d 天内到期", days+1)})
				}
			}
		}
	}

	for _, n := range nodes {
		if n.Status == node.StatusDisabled {
			continue
		}
		if n.Status == node.StatusDeployFailed {
			alerts = append(alerts, Alert{AlertError, "node", n.Name, n.ID, "上次部署失败"})
		}
		// 从未采集过的节点不报警:刚加的节点本来就还没有数据,
		// 报出来只是噪声。有过数据又断了才值得看一眼。
		m, ok := metrics[n.ID]
		if !ok {
			continue
		}
		collected, err := time.Parse(time.RFC3339, m.CollectedAt)
		if err != nil || now.Sub(collected) > metricsStaleAfter {
			alerts = append(alerts, Alert{AlertWarning, "node", n.Name, n.ID,
				"监控数据已超过 10 分钟未更新"})
		}
	}

	// error 排在 warning 前面,同级按目标名排序,保证刷新页面时顺序稳定。
	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].Level != alerts[j].Level {
			return alerts[i].Level == AlertError
		}
		if alerts[i].Category != alerts[j].Category {
			return alerts[i].Category < alerts[j].Category
		}
		return alerts[i].Target < alerts[j].Target
	})
	return alerts
}

func (s *Server) handleDashboardAlerts(w http.ResponseWriter, r *http.Request) {
	users, err := s.users.Store().List(r.Context())
	if err != nil {
		s.logger.Error("查询用户列表失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	var nodes []*node.Node
	if s.nodes != nil {
		if nodes, err = s.nodes.Store().List(r.Context()); err != nil {
			s.logger.Error("查询节点列表失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
	}

	// 监控是可以在配置里整个关掉的能力,取不到就当没有采样数据,
	// 不能让它拖垮整个预警列表。
	metrics := map[int64]node.Metrics{}
	if s.metrics != nil {
		if loaded, err := s.metrics.Latest(r.Context()); err != nil {
			s.logger.Error("查询节点资源采样失败", "error", err)
		} else {
			metrics = loaded
		}
	}

	alerts := buildDashboardAlerts(users, nodes, metrics, time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]any{"items": alerts})
}
