package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/node"
)

const actionNodeRecheck = "node.recheck"

// handleRecheckNode 只读地把一台机器重新核验一遍(V20)。
//
// 管理地址改完之后前端自动调它;节点详情菜单里也有一个手动入口。
// node 包管连接、转发、服务、配置、端口与探测档案;流量采集与资源采集两步
// 挂在调度器与监控器上,在这里补齐 —— 检查范围是这台机器实际承载的服务。
func (s *Server) handleRecheckNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	result, err := s.nodes.Recheck(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "重新核验节点失败")
		return
	}
	if result.OK || sshReached(result) {
		n, nerr := s.nodes.Store().Get(r.Context(), id)
		if nerr == nil {
			result.Steps = append(result.Steps, s.recheckTrafficStep(r, n))
			result.Steps = append(result.Steps, s.recheckMetricsStep(r, n))
		}
	}
	for _, st := range result.Steps {
		if st.Status == deployment.StepFailed {
			result.OK = false
		}
	}
	result.FinishedAt = time.Now().UTC()
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeRecheck,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   recheckSummary(result),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: result.OK,
	})
	writeJSON(w, http.StatusOK, result)
}

func sshReached(res node.RecheckResult) bool {
	for _, it := range res.Preflight.Items {
		if it.Key == "ssh" {
			return it.Status == node.CheckSatisfied
		}
	}
	return false
}

// recheckTrafficStep 试着同步一次流量:它验的是 V2Ray API 那条通道。
func (s *Server) recheckTrafficStep(r *http.Request, n *node.Node) deployment.Step {
	start := time.Now()
	step := deployment.Step{Name: "流量采集", Status: deployment.StepSkipped}
	switch {
	case n.Role.IsRelay():
		step.Detail = "中转主机,面板不计流量"
	case n.DeployedConfigSHA256 == "" && len(n.MieruInbounds) == 0:
		step.Detail = "还没下发过配置,没有计数器可读"
	case s.scheduler == nil:
		step.Detail = "面板没有接入流量同步"
	default:
		res, err := s.scheduler.SyncNodeNow(r.Context(), n.ID)
		step.DurationMS = time.Since(start).Milliseconds()
		if err != nil {
			step.Status = deployment.StepFailed
			step.Detail = err.Error()
		} else {
			step.Status = deployment.StepSuccess
			step.Detail = fmt.Sprintf("已同步:读到 %d 个计数器,入账 %d 条", res.CountersRead, res.EntriesAdded)
		}
	}
	return step
}

// recheckMetricsStep 采一次资源样本:它验的是资源采集那条 SSH 通道,
// 顺带让"变更前数据"的标记消失 —— 新地址上有了第一份样本。
func (s *Server) recheckMetricsStep(r *http.Request, n *node.Node) deployment.Step {
	start := time.Now()
	step := deployment.Step{Name: "资源采集", Status: deployment.StepSkipped}
	if s.monitor == nil {
		step.Detail = "面板关闭了资源采集"
		return step
	}
	m, err := s.monitor.CollectNode(r.Context(), n.ID)
	step.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		step.Status = deployment.StepFailed
		step.Detail = err.Error()
		return step
	}
	step.Status = deployment.StepSuccess
	step.Detail = fmt.Sprintf("CPU %.0f%%,内存 %d/%d MB", m.CPUPercent, m.MemUsedKB/1024, m.MemTotalKB/1024)
	return step
}

func recheckSummary(res node.RecheckResult) string {
	failed := 0
	for _, st := range res.Steps {
		if st.Status == deployment.StepFailed {
			failed++
		}
	}
	if failed == 0 {
		return fmt.Sprintf("全部 %d 项通过", len(res.Steps))
	}
	return fmt.Sprintf("%d 项未通过:%s", failed, res.Preflight.BlockReason)
}
