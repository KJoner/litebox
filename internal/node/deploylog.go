package node

import (
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
)

// 一次部署的结局必须落进系统日志。
//
// 部署的全部过程只存在于 deployment.Result.Steps 里,而它只经
// deployStore.Save 落库 —— Save 一失败(数据库锁、磁盘满、ctx 被取消),
// 这次部署就再也没有任何痕迹可查。
//
// **已经发生过**:一次 chain_apply 被反代的读超时掐断,journal 里只剩
// 三行 context canceled,连"卡在哪一步"都答不出来,而节点上的 sing-box
// 确实重启过。ctx 那一侧已经修了,但 Save 还有别的失败方式,而部署恰恰是
// 最不能没有痕迹的那种操作 —— 它重启服务、踢掉全部在线连接。

// logDeployResult 把一次部署的结局写进系统日志。
//
// 只写"哪一步失败"与错误的第一行,**不写完整 detail**:拨测失败时它带着
// 节点上 sing-box 的日志原文,而那里面可能有用户 UUID。完整内容留在部署
// 记录里 —— 那是有访问控制的地方,而 journal 通常谁都读得到。
func logDeployResult(
	logger *slog.Logger, nodeID int64, result deployment.Result, err error,
) {
	if logger == nil {
		return
	}
	what := "部署"
	switch result.Kind {
	case deployment.KindRelay:
		what = "中转转发下发"
	case deployment.KindRealm:
		// 单独一个词:它 restart、断开在途连接,与只 reload 的 nginx 不是一回事。
		what = "realm 转发下发"
	}
	took := result.FinishedAt.Sub(result.StartedAt).Round(time.Millisecond)
	// FinishedAt 是零值说明结果没走完 finish(渲染阶段就失败了),
	// 这时报一个几万小时的耗时比不报更糟。
	if result.FinishedAt.IsZero() || result.StartedAt.IsZero() {
		took = 0
	}

	if err == nil {
		logger.Info(what+"成功", "node_id", nodeID, "revision", result.Revision,
			"耗时", took.String())
		return
	}
	attrs := []any{
		"node_id", nodeID,
		"revision", result.Revision,
		"失败步骤", failedStepName(result),
		"error", firstLine(err.Error(), 300),
		"耗时", took.String(),
	}
	// 回滚结果是这条日志里最要紧的一项:它回答的是"节点现在还能不能用",
	// 而那与"这次部署失败了"是两个问题。
	if result.RollbackResult != "" {
		attrs = append(attrs, "回滚", result.RollbackResult)
	}
	logger.Error(what+"失败", attrs...)
}

// failedStepName 返回第一个失败的步骤名。
//
// 只要名字不要 detail —— 有了它管理员就知道该去哪看:是拨测、端口监听,
// 还是 sing-box check。没有失败步骤(比如渲染阶段就返回了)时说清楚,
// 而不是留一个空字符串让人以为日志本身出了问题。
func failedStepName(result deployment.Result) string {
	for _, s := range result.Steps {
		if s.Status == deployment.StepFailed {
			return s.Name
		}
	}
	return "(未进入步骤记录)"
}

// firstLine 取错误的第一行并截断。
//
// 部署错误经常是多行的(拨测会把节点日志带回来),整段倒进 slog 的一个
// 属性里既难读又会把凭据带出去。
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …(完整内容见部署记录)"
	}
	// 按 rune 截而不是按字节:错误信息几乎都是中文,按字节切会把最后一个字
	// 劈成半个,journal 里显示成一个乱码方块。
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
