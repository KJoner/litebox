package nodeport

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/mieru"
)

// 端口冲突检测的核心是**区间对区间**:单值只是 Start = End 的一段。
// 这些用例盯着四类占用者两两之间的每一种组合 —— 漏掉任何一种的表现都是
// 同一个:其中一个服务 bind 失败、整个起不来,而要到部署的健康检查才发现。

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "p.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

// seed 造一台落地机器,API 端口固定 28080。
func seed(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`
		INSERT INTO nodes (name, display_name, host, ssh_port, ssh_user,
			ssh_key_encrypted, ssh_host_key, api_port,
			proxy_port, listen_port, ipv6_proxy_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			sort_order, traffic_quota_bytes, traffic_reset_cycle, traffic_reset_day,
			traffic_billing_mode, role, status, created_at, updated_at)
		VALUES ('n1','n1','192.0.2.1',22,'root','','',28080,0,0,0,'','','','',
		        0,0,'NONE',1,'EGRESS','LANDING','PENDING',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func addInbound(t *testing.T, db *sql.DB, nodeID int64, port int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			created_at, updated_at)
		VALUES (?, ?, 'in', 'VLESS_REALITY', ?, ?, ?)`,
		nodeID, "in-"+time.Now().Format("150405.000000000"), port, now, now); err != nil {
		t.Fatal(err)
	}
}

func addMieru(t *testing.T, db *sql.DB, nodeID int64, start, end int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		INSERT INTO node_mieru_inbounds (node_id, display_name,
			listen_port_start, listen_port_end, created_at, updated_at)
		VALUES (?, 'm', ?, ?, ?, ?)`, nodeID, start, end, now, now); err != nil {
		t.Fatal(err)
	}
}

func free(t *testing.T, db *sql.DB, nodeID int64, start, end int) error {
	t.Helper()
	return Free(context.Background(), db, nodeID,
		mieru.PortRange{Start: start, End: end}, Skip{})
}

// 单端口撞上单端口。
func TestSinglePortAgainstInbound(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	addInbound(t, db, id, 24443)

	if err := free(t, db, id, 24443, 24443); !errors.Is(err, ErrConflict) {
		t.Errorf("同端口应当冲突,得到 %v", err)
	}
	if err := free(t, db, id, 24444, 24444); err != nil {
		t.Errorf("不同端口不该冲突:%v", err)
	}
}

// API 端口也要避开:那一个端口上也有东西在听。
func TestSinglePortAgainstAPIPort(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	if err := free(t, db, id, 28080, 28080); !errors.Is(err, ErrConflict) {
		t.Errorf("API 端口应当冲突,得到 %v", err)
	}
}

// **一段端口跨过 API 端口时同样要拦。**
// 这一条正是原来那种 `listen_port = ?` 写法查不出来的:
// 端口段 28000-28100 里含着 28080,而两者没有任何一个"相等"。
func TestRangeSwallowingAPIPort(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	if err := free(t, db, id, 28000, 28100); !errors.Is(err, ErrConflict) {
		t.Errorf("跨过 API 端口的段应当冲突,得到 %v", err)
	}
}

// 一段端口罩住一个已有入站。
func TestRangeSwallowingInbound(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	addInbound(t, db, id, 30005)

	if err := free(t, db, id, 30000, 30010); !errors.Is(err, ErrConflict) {
		t.Errorf("罩住已有入站的段应当冲突,得到 %v", err)
	}
	if err := free(t, db, id, 31000, 31010); err != nil {
		t.Errorf("不相交的段不该冲突:%v", err)
	}
}

// 反过来:一个新入站落进已有的 Mieru 段里。
// 这是新加的方向 —— 原来那两处实现根本不查 node_mieru_inbounds。
func TestSinglePortInsideMieruRange(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	addMieru(t, db, id, 30000, 30010)

	if err := free(t, db, id, 30005, 30005); !errors.Is(err, ErrConflict) {
		t.Errorf("落进 Mieru 段的端口应当冲突,得到 %v", err)
	}
	if err := free(t, db, id, 30011, 30011); err != nil {
		t.Errorf("段外的端口不该冲突:%v", err)
	}
}

// 两段相交的每一种形状都要判出来。
//
// **判据是 a.start <= b.end && b.start <= a.end,不是"端点落在里面"** ——
// 后者漏判【包含】的情形(一段完全套在另一段里时,两个端点都不落在
// 对方的端点上),而那正是"把一个已有段改窄"最容易造出来的形状。
func TestRangeOverlapShapes(t *testing.T) {
	cases := []struct {
		name       string
		start, end int
		conflict   bool
	}{
		{"完全相同", 30000, 30010, true},
		{"左边搭上", 29990, 30000, true},
		{"右边搭上", 30010, 30020, true},
		{"完全套在里面", 30003, 30006, true},
		{"完全罩住", 29000, 31000, true},
		{"紧挨着不重叠(左)", 29990, 29999, false},
		{"紧挨着不重叠(右)", 30011, 30020, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newDB(t)
			id := seed(t, db)
			addMieru(t, db, id, 30000, 30010)

			err := free(t, db, id, c.start, c.end)
			if c.conflict && !errors.Is(err, ErrConflict) {
				t.Errorf("应当冲突,得到 %v", err)
			}
			if !c.conflict && err != nil {
				t.Errorf("不该冲突:%v", err)
			}
		})
	}
}

// 编辑自己时要放过自己,**而且只放过同一类里的自己**。
//
// 只按 id 排除的话,编辑 3 号入站会顺带放过 3 号转发规则与 3 号 Mieru 入口,
// 于是一次真实的冲突被静默放行。
func TestSkipOnlyMatchesItsOwnKind(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	addMieru(t, db, id, 30000, 30010)

	var mieruID int64
	if err := db.QueryRow(`SELECT id FROM node_mieru_inbounds`).Scan(&mieruID); err != nil {
		t.Fatal(err)
	}
	rng := mieru.PortRange{Start: 30000, End: 30010}
	ctx := context.Background()

	if err := Free(ctx, db, id, rng, Skip{Kind: KindMieru, ID: mieruID}); err != nil {
		t.Errorf("编辑自己不该报冲突:%v", err)
	}
	// 同一个 id 但换一类:必须仍然判成冲突。
	if err := Free(ctx, db, id, rng, Skip{Kind: KindInbound, ID: mieruID}); !errors.Is(err, ErrConflict) {
		t.Errorf("换一类之后应当仍然冲突,得到 %v", err)
	}
}

// 空段表示"跟随",还没落到具体号码上,直接放行。
func TestEmptyRangeIsAlwaysFree(t *testing.T) {
	db := newDB(t)
	id := seed(t, db)
	addMieru(t, db, id, 30000, 30010)

	if err := Free(context.Background(), db, id, mieru.PortRange{}, Skip{}); err != nil {
		t.Errorf("空段不该报冲突:%v", err)
	}
}
