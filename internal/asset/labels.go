// Package asset 管资产卡片上的「地区」与「标签」:只用于仪表盘的筛选与展示,
// 不进节点配置、不进订阅、不影响任何权限。
//
// 对象仍然是到期档案那三类(自建节点、外部代理、代理源),键也一样,
// 但**刻意不与 expiry_profiles 合表**:外部代理一旦有了自己的档案行就不再跟随
// 来源的到期信息,只想打个标签却悄悄改变了到期继承,那是一个没有任何提示的副作用。
package asset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/expiry"
)

// Labels 是一个对象的地区与标签。
type Labels struct {
	Kind     expiry.Kind `json:"kind"`
	ObjectID int64       `json:"object_id"`
	// Region 是 ISO 3166-1 alpha-2 大写,空串 = 未设置。面板不按名字猜地区。
	Region string   `json:"region"`
	Tags   []string `json:"tags"`
}

// Key 定位一个对象。
type Key struct {
	Kind     expiry.Kind
	ObjectID int64
}

const (
	maxTags   = 8
	maxTagLen = 16
)

// NormalizeRegion 校验地区代码:两位字母,统一大写;空串表示未设置。
func NormalizeRegion(raw string) (string, error) {
	r := strings.ToUpper(strings.TrimSpace(raw))
	if r == "" {
		return "", nil
	}
	if len(r) != 2 || r[0] < 'A' || r[0] > 'Z' || r[1] < 'A' || r[1] > 'Z' {
		return "", fmt.Errorf("地区 %q 必须是两位字母的国家 / 地区代码(如 HK、JP、US)", raw)
	}
	return r, nil
}

// NormalizeTags 去空白、去重(不分大小写,保留第一次出现的写法)、排序。
func NormalizeTags(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if strings.ContainsAny(t, ",，\n\r\t") {
			return nil, fmt.Errorf("标签 %q 里不能有逗号或换行", t)
		}
		if len([]rune(t)) > maxTagLen {
			return nil, fmt.Errorf("标签 %q 最长 %d 个字符", t, maxTagLen)
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, fmt.Errorf("标签最多 %d 个", maxTags)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out, nil
}

func splitTags(raw string) []string {
	out := []string{}
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Store 读写 asset_labels。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// All 一次读出全部标签,给仪表盘聚合接口用。
func (s *Store) All(ctx context.Context) (map[Key]Labels, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, object_id, region, tags FROM asset_labels`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]Labels{}
	for rows.Next() {
		var l Labels
		var tags string
		if err := rows.Scan(&l.Kind, &l.ObjectID, &l.Region, &tags); err != nil {
			return nil, err
		}
		l.Tags = splitTags(tags)
		out[Key{l.Kind, l.ObjectID}] = l
	}
	return out, rows.Err()
}

// Get 读一个对象的标签;没有时返回空标签(不是错误)。
func (s *Store) Get(ctx context.Context, key Key) (Labels, error) {
	l := Labels{Kind: key.Kind, ObjectID: key.ObjectID, Tags: []string{}}
	var tags string
	err := s.db.QueryRowContext(ctx,
		`SELECT region, tags FROM asset_labels WHERE kind = ? AND object_id = ?`,
		key.Kind, key.ObjectID).Scan(&l.Region, &tags)
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	l.Tags = splitTags(tags)
	return l, nil
}

// Save 整体覆盖一个对象的标签。两项都为空时删掉那一行 —— 不留一行全空的记录。
func (s *Store) Save(ctx context.Context, key Key, region string, tags []string) (Labels, error) {
	r, err := NormalizeRegion(region)
	if err != nil {
		return Labels{}, err
	}
	t, err := NormalizeTags(tags)
	if err != nil {
		return Labels{}, err
	}
	if r == "" && len(t) == 0 {
		return Labels{Kind: key.Kind, ObjectID: key.ObjectID, Tags: []string{}}, s.Delete(ctx, key)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO asset_labels (kind, object_id, region, tags, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(kind, object_id) DO UPDATE SET
		  region = excluded.region, tags = excluded.tags, updated_at = excluded.updated_at`,
		key.Kind, key.ObjectID, r, strings.Join(t, ","), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return Labels{}, err
	}
	return Labels{Kind: key.Kind, ObjectID: key.ObjectID, Region: r, Tags: t}, nil
}

// Delete 删掉一个对象的标签(对象被删除时顺手调)。
func (s *Store) Delete(ctx context.Context, key Key) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM asset_labels WHERE kind = ? AND object_id = ?`, key.Kind, key.ObjectID)
	return err
}
