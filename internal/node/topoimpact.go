package node

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/relay"
)

// 依赖传播的统一实现(v23.3)。在此之前「改 X 要重新下发谁」散在四个函数里,各自只认识一种下一跳:
// 转发规则只查指向入站的、链式只查指向入站的、Mieru 又一份。转发可以指向另一条转发之后,那种写法
// 必然会漏 —— 漏掉的表现永远是同一个:上游拿着一套过时的参数去连,而面板上每台机器都显示正常。
//
// 两种依赖分开算,因为它们要下发的组件不同:
//   - 拨号依赖:谁直接连着这个入口的某条地址。入口的地址或端口变了,只有**直接**上游的配置字节会变
//     (转发规则的 proxy_pass / remote、链式出站的 server);
//   - 认证依赖:谁经透明转发最终在这个入口认证。入口的协议参数变了、或者一条转发换了去向,
//     那条路径上**所有**代理发起方的出站参数都可能变(它们用认证终点的协议与凭据)。
// 多标的那几台由「配置已一致就不重启」兜住(Result.Unchanged);转发规则只按拨号依赖标 ——
// realm 每次下发都要重启,白标一次就是白断一次在途连接。

// entriesOnNode 列出一台机器上能被别人当下一跳的入口(sing-box 入站与转发规则)。
func (s *Store) entriesOnNode(ctx context.Context, q chaingraph.Queryer, nodeID int64) ([]chaingraph.Ref, error) {
	var out []chaingraph.Ref
	for _, qq := range []struct {
		sql  string
		kind chaingraph.Kind
	}{
		{`SELECT id, '' FROM node_inbounds WHERE node_id = ? AND deleted_at IS NULL`, chaingraph.KindSingBox},
		{`SELECT id, engine FROM node_relays WHERE node_id = ? AND deleted_at IS NULL`, chaingraph.KindNginx},
	} {
		rows, err := q.QueryContext(ctx, qq.sql, nodeID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var engine string
			if err := rows.Scan(&id, &engine); err != nil {
				rows.Close()
				return nil, err
			}
			k := qq.kind
			if engine == "REALM" {
				k = chaingraph.KindRealm
			}
			out = append(out, chaingraph.Ref{Kind: k, ID: id})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// impact 是一次改动要重新下发的组件:sing-box(含 Mieru 出口那一跳)与两种转发引擎,按机器。
type impact struct {
	singbox map[int64]bool
	relays  map[int64]bool
	realm   map[int64]bool
	// entries 是会受影响的入口(只读摘要可能变的也算),给预览与报错用。
	entries map[chaingraph.Ref]chaingraph.Referrer
}

func newImpact() *impact {
	return &impact{singbox: map[int64]bool{}, relays: map[int64]bool{}, realm: map[int64]bool{},
		entries: map[chaingraph.Ref]chaingraph.Referrer{}}
}

// addDial 记一个直接连着改动对象的上游:它自己的配置字节会变。
func (im *impact) addDial(r chaingraph.Referrer) {
	im.entries[r.Ref] = r
	switch r.Ref.Kind {
	case chaingraph.KindSingBox, chaingraph.KindMieru:
		im.singbox[r.NodeID] = true
	case chaingraph.KindNginx:
		im.relays[r.NodeID] = true
	case chaingraph.KindRealm:
		im.realm[r.NodeID] = true
	}
}

// addAuth 记一个经转发在改动对象处认证的上游:代理发起方的出站参数会变;转发规则只是摘要变,不下发。
func (im *impact) addAuth(r chaingraph.Referrer) {
	im.entries[r.Ref] = r
	if r.Ref.Kind == chaingraph.KindSingBox || r.Ref.Kind == chaingraph.KindMieru {
		im.singbox[r.NodeID] = true
	}
}

// collect 算出改动 refs 之后的影响:直接引用者按拨号依赖,全部上游按认证依赖。
func (s *Store) collectImpact(ctx context.Context, q chaingraph.Queryer, refs []chaingraph.Ref) (*impact, error) {
	im := newImpact()
	for _, ref := range refs {
		direct, err := chaingraph.Referrers(ctx, q, ref)
		if err != nil {
			return nil, err
		}
		for _, r := range direct {
			im.addDial(r)
		}
		ups, err := chaingraph.Upstream(ctx, q, ref)
		if err != nil {
			return nil, err
		}
		for _, r := range ups {
			im.addAuth(r)
		}
	}
	return im, nil
}

// apply 把影响交给协调器标脏(不在事务里:调用方的改动已经提交;认证相关的那几条走 recordIntents)。
func (s *Service) applyImpact(im *impact) {
	if s.trigger == nil || im == nil {
		return
	}
	if ids := keys(im.singbox); len(ids) > 0 {
		s.trigger.MarkDirty(ids...)
	}
	if ids := keys(im.relays); len(ids) > 0 {
		s.trigger.MarkRelaysDirty(ids...)
	}
	if ids := keys(im.realm); len(ids) > 0 {
		s.trigger.MarkRealmDirty(ids...)
	}
}

// recordIntents 在调用方的事务里把影响写进持久化下发队列(G01)。
func recordIntents(ctx context.Context, tx *sql.Tx, im *impact) error {
	if err := applyintent.Record(ctx, tx, applyintent.SingBox, keys(im.singbox)...); err != nil {
		return err
	}
	if err := applyintent.Record(ctx, tx, applyintent.Relays, keys(im.relays)...); err != nil {
		return err
	}
	return applyintent.Record(ctx, tx, applyintent.Realm, keys(im.realm)...)
}

func (s *Service) wakeImpact(im *impact) {
	s.wake(applyintent.SingBox, keys(im.singbox)...)
	s.wake(applyintent.Relays, keys(im.relays)...)
	s.wake(applyintent.Realm, keys(im.realm)...)
}

func keys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PropagateEndpointChange 在几条地址条目的地址或公网端口变了之后,把明确选了它们的上游标脏。
// 未被选中的条目变化不传播:它不是任何链路的目的地址(A05)。
func (s *Service) PropagateEndpointChange(ctx context.Context, endpointIDs []int64) {
	if s.trigger == nil || len(endpointIDs) == 0 {
		return
	}
	im := newImpact()
	for _, id := range endpointIDs {
		refs, err := chaingraph.EndpointReferrers(ctx, s.store.db, id)
		if err != nil {
			s.logger.Error("查询引用这条地址的入口失败", "endpoint_id", id, "error", err)
			continue
		}
		for _, r := range refs {
			im.addDial(r)
		}
	}
	s.applyImpact(im)
}

// relayHook 实现 relay.ChangeHook:一条转发规则改动的同一个事务里,算出经它的代理发起方认证终点有没有换,
// 换了就在旧终点上保留凭据、并把新终点与发起方记成要重新下发。
type relayHook struct{ s *Service }

// RelayChangeHook 交给 relay.Store.SetChangeHook。
func (s *Service) RelayChangeHook() relay.ChangeHook { return relayHook{s} }

type relayHookState struct {
	sources   []hookSource
	listen    int
	public    int
	nodeID    int64
	kind      chaingraph.Kind
	directUps []chaingraph.Referrer
}

type hookSource struct {
	ref     chaingraph.Ref
	nodeID  int64
	code    string
	landing int64
}

func (h relayHook) BeforeRelayChange(ctx context.Context, tx *sql.Tx, relayID int64) (any, error) {
	ref := chaingraph.Ref{Kind: chaingraph.KindNginx, ID: relayID}
	self, err := chaingraph.Load(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	st := &relayHookState{listen: self.ListenPort, public: self.PublicPort, nodeID: self.NodeID, kind: self.Kind}
	ups, err := chaingraph.Upstream(ctx, tx, self.Ref)
	if err != nil {
		return nil, err
	}
	for _, u := range ups {
		if u.Ref.Kind != chaingraph.KindSingBox && u.Ref.Kind != chaingraph.KindMieru {
			continue
		}
		e, err := chaingraph.Load(ctx, tx, u.Ref)
		if err != nil {
			continue
		}
		landing, err := authLandingOf(ctx, tx, e.Next)
		if err != nil {
			return nil, err
		}
		st.sources = append(st.sources, hookSource{ref: u.Ref, nodeID: e.NodeID, code: e.ChainCode, landing: landing})
	}
	if st.directUps, err = chaingraph.Referrers(ctx, tx, self.Ref); err != nil {
		return nil, err
	}
	return st, nil
}

func (h relayHook) AfterRelayChange(ctx context.Context, tx *sql.Tx, relayID int64, state any) error {
	st, _ := state.(*relayHookState)
	if st == nil {
		return nil
	}
	self, err := chaingraph.Load(ctx, tx, chaingraph.Ref{Kind: st.kind, ID: relayID})
	if err != nil {
		return err
	}
	im := newImpact()
	// 这条规则自己那台机器的引擎(转发配置变了);与 handler 里的标脏重复无妨,队列按 seq 合并。
	if self.Kind == chaingraph.KindRealm {
		im.realm[self.NodeID] = true
	} else {
		im.relays[self.NodeID] = true
	}
	// 端口变了:直接连它的上游拨的地址跟着变(地址条目的端口 0 跟随规则的公网端口)。
	if self.ListenPort != st.listen || self.PublicPort != st.public {
		for _, r := range st.directUps {
			im.addDial(r)
		}
	}
	for _, src := range st.sources {
		e, err := chaingraph.Load(ctx, tx, src.ref)
		if err != nil {
			continue
		}
		landing, err := authLandingOf(ctx, tx, e.Next)
		if err != nil {
			return err
		}
		// 经这条规则的代理发起方:出站参数取认证终点的,规则一改就可能变 —— 都标上,配置一致的不会重启。
		im.singbox[src.nodeID] = true
		if landing != src.landing {
			if err := noteAuthChange(ctx, tx, src.ref.Kind, src.ref.ID, src.nodeID, src.code, src.landing, landing); err != nil {
				return err
			}
			if landing != 0 {
				var landingNode int64
				if err := tx.QueryRowContext(ctx, `SELECT node_id FROM node_inbounds WHERE id = ?`, landing).Scan(&landingNode); err == nil {
					im.singbox[landingNode] = true
				}
			}
		}
	}
	if err := recordIntents(ctx, tx, im); err != nil {
		return err
	}
	// 提交之后再叫醒协调器:这里还在事务里,叫早了它会读到改动之前的队列。
	h.s.pendingWake(im)
	return nil
}

// pendingWake 把要叫醒的组件挂起,等调用方的事务提交之后由 FlushWake 叫醒。
// 转发规则的改动由 HTTP 层在 Update 返回之后调用 FlushWake;忘了调也只是晚 30 秒(协调器会从库里捞)。
func (s *Service) pendingWake(im *impact) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeQueue == nil {
		s.wakeQueue = newImpact()
	}
	for k := range im.singbox {
		s.wakeQueue.singbox[k] = true
	}
	for k := range im.relays {
		s.wakeQueue.relays[k] = true
	}
	for k := range im.realm {
		s.wakeQueue.realm[k] = true
	}
}

// FlushWake 叫醒协调器去做 pendingWake 攒下的那些。
func (s *Service) FlushWake() {
	s.wakeMu.Lock()
	im := s.wakeQueue
	s.wakeQueue = nil
	s.wakeMu.Unlock()
	if im != nil {
		s.wakeImpact(im)
	}
}

// ReferencedError 表示还有入口把要删的东西(入口、地址条目、地址池里的地址)当下一跳的目的地。
// 删除默认拒绝并说清是谁(A15):删掉之后那些入口要么渲染不出来,要么被悄悄改成直连或别的地址 —— 两种都不能发生。
type ReferencedError struct {
	What      string
	Referrers []chaingraph.Referrer
}

func (e *ReferencedError) Error() string {
	parts := make([]string, 0, len(e.Referrers))
	for _, r := range e.Referrers {
		parts = append(parts, r.Describe())
	}
	return fmt.Sprintf("%s还被这些入口当成下一跳,先给它们重选目标或解除关系:%s", e.What, strings.Join(parts, ";"))
}

// externalReferrers 列出把下一跳指向别的机器上的入口的那些引用(删一台机器之前用:同机的引用随机器一起没了)。
func (s *Store) externalReferrersOfNode(ctx context.Context, nodeID int64) ([]chaingraph.Referrer, error) {
	refs, err := s.entriesOnNode(ctx, s.db, nodeID)
	if err != nil {
		return nil, err
	}
	var out []chaingraph.Referrer
	for _, ref := range refs {
		rs, err := chaingraph.Referrers(ctx, s.db, ref)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.NodeID != nodeID {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// CheckNodeDeletable 在删一台机器之前确认别的机器上没有入口还以它上面的入口为下一跳。
func (s *Store) CheckNodeDeletable(ctx context.Context, nodeID int64) error {
	refs, err := s.externalReferrersOfNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &ReferencedError{What: "这台机器上的入口", Referrers: refs}
	}
	return nil
}

// CheckEntryDeletable 在删一个入口之前确认没有别的入口还以它为下一跳。
func (s *Store) CheckEntryDeletable(ctx context.Context, ref chaingraph.Ref) error {
	refs, err := chaingraph.Referrers(ctx, s.db, ref)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &ReferencedError{What: "这个入口", Referrers: refs}
	}
	return nil
}
