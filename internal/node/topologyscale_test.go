package node

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/singbox"
)

// v23.3 画布的性能验收(A28):固定的 100 / 300 个入口的数据集,含共享中继、独立分量与多地址卡片,
// 记录快照接口在库里批量读的耗时。默认跳过;设 LITEBOX_TOPOBENCH=1 才跑(它要建几百行数据)。
// 数据全部是文档地址段。
func TestTopologySnapshotScale(t *testing.T) {
	if os.Getenv("LITEBOX_TOPOBENCH") == "" {
		t.Skip("设 LITEBOX_TOPOBENCH=1 才运行")
	}
	for _, total := range []int{100, 300} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			store, _ := newTestStore(t)
			svc := NewService(ServiceOptions{Store: store, Users: &batchStubUsers{},
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			ctx := t.Context()
			rs := relay.NewStore(store.db)
			// 每台机器 5 个入口:3 个 sing-box(第一个随建机器生成)、2 条转发;每 10 台一组:
			// 前 8 台的第二个入口链到本组第 9 台的一条共享转发,共享转发再到第 10 台的落地;其余是独立入口。
			hosts := total / 5
			type hostRec struct {
				id       int64
				inbounds []int64
				relays   []int64
			}
			recs := make([]hostRec, hosts)
			for h := 0; h < hosts; h++ {
				p := defaultCreateParams()
				p.Name = fmt.Sprintf("n%03d", h)
				p.Host = fmt.Sprintf("192.0.2.%d", h%250+1)
				n, err := store.Create(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				rec := hostRec{id: n.ID, inbounds: []int64{n.Inbounds[0].ID}}
				for k := 1; k < 3; k++ {
					in, err := store.CreateInbound(ctx, n.ID, InboundParams{DisplayName: fmt.Sprintf("in-%d-%d", h, k),
						Protocol: "SHADOWSOCKS", SSMethod: "2022-blake3-aes-128-gcm", ListenPort: 20000 + k})
					if err != nil {
						t.Fatal(err)
					}
					rec.inbounds = append(rec.inbounds, in.ID)
				}
				// 多地址卡片:第一个入口三条地址(管理地址 + 两条额外地址)。
				if _, err := store.ReplaceAddresses(ctx, n.ID, []AddressInput{
					{Family: AddrFamilyV4, Address: fmt.Sprintf("198.51.100.%d", h%250+1)},
					{Family: AddrFamilyV6, Address: fmt.Sprintf("2001:db8::%x", h+1)},
				}); err != nil {
					t.Fatal(err)
				}
				addrs, _ := store.AddressesForNode(ctx, n.ID)
				if _, err := store.ReplaceEndpoints(ctx, n.ID, EndpointKindSingBox, rec.inbounds[0], []EndpointInput{
					{PublicPort: 443}, {AddressID: &addrs[0].ID, PublicPort: 2443}, {AddressID: &addrs[1].ID, PublicPort: 3443},
				}, false); err != nil {
					t.Fatal(err)
				}
				for k := 0; k < 2; k++ {
					r, err := rs.Create(ctx, relay.CreateParams{NodeID: n.ID, DisplayName: fmt.Sprintf("r-%d-%d", h, k),
						ListenPort: 30000 + k, TargetKind: "ADDRESS", TargetHost: "198.51.100.200", TargetPort: 8443})
					if err != nil {
						t.Fatal(err)
					}
					rec.relays = append(rec.relays, r.ID)
				}
				recs[h] = rec
			}
			if _, err := store.db.Exec(`UPDATE node_inbounds SET deployed_protocol = protocol, deployed_ss_method = ss_method`); err != nil {
				t.Fatal(err)
			}
			for g := 0; g+9 < hosts; g += 10 {
				landing := recs[g+9].inbounds[0]
				shared := recs[g+8].relays[0]
				cur, _ := rs.Get(ctx, shared)
				// 共享中继改指本组落地(指定地址 → 入站要新建一条,这里直接写库,与界面上「新建一条」等价)。
				if _, err := store.db.Exec(`UPDATE node_relays SET target_kind = 'INBOUND', target_inbound_id = ?, target_host = '',
					target_port = 0, target_addr_mode = 'ENDPOINT', target_endpoint_id = (SELECT MIN(id) FROM inbound_endpoints
					WHERE entry_kind = 'SINGBOX' AND entry_id = ?) WHERE id = ?`, landing, landing, cur.ID); err != nil {
					t.Fatal(err)
				}
				for k := 0; k < 8; k++ {
					if err := store.SetChain(ctx, recs[g+k].inbounds[1], ChainTargetRelay, shared); err != nil {
						t.Fatalf("链到共享转发: %v", err)
					}
				}
			}

			// 标成已部署:快照里的配置状态要真的把每台机器的配置渲染一遍(与节点列表同一个判据),那才是生产上的开销。
			for _, r := range recs {
				markDeployedWithCurrent(t, svc, r.id)
			}
			var best time.Duration
			for i := 0; i < 3; i++ {
				started := time.Now()
				snap, err := svc.Topology(ctx)
				if err != nil {
					t.Fatal(err)
				}
				el := time.Since(started)
				if i == 0 || el < best {
					best = el
				}
				if len(snap.Entries) != total {
					t.Fatalf("应有 %d 张卡片,得到 %d", total, len(snap.Entries))
				}
			}
			nodes, _ := store.List(ctx)
			started := time.Now()
			svc.ConfigStatuses(ctx, nodes)
			cs := time.Since(started)
			started = time.Now()
			if _, err := store.TopologyRevision(ctx); err != nil {
				t.Fatal(err)
			}
			t.Logf("%d 个入口:快照 %v(三次里最快的一次);其中配置状态 %v、修订 %v", total, best, cs, time.Since(started))
		})
	}
}

// batchStubUsers 带上批量版,让配置状态走与生产相同的 prefetchRender 路径(一次取齐全部入口的用户)。
type batchStubUsers struct{ stubUsers }

func (b *batchStubUsers) UsersForInbounds(_ context.Context, ids []int64) (map[int64][]singbox.User, error) {
	out := make(map[int64][]singbox.User, len(ids))
	for _, id := range ids {
		out[id] = b.users
	}
	return out, nil
}
