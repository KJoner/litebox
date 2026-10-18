package asset

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/expiry"
)

func TestNormalize(t *testing.T) {
	if r, err := NormalizeRegion(" hk "); err != nil || r != "HK" {
		t.Errorf("region = %q %v", r, err)
	}
	for _, bad := range []string{"HKG", "1A", "香港"} {
		if _, err := NormalizeRegion(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	got, err := NormalizeTags([]string{" 生产 ", "CN2", "cn2", "", "BGP"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"BGP", "CN2", "生产"}) {
		t.Errorf("tags = %v", got)
	}
	if _, err := NormalizeTags([]string{"a,b"}); err == nil {
		t.Error("带逗号的标签应被拒绝")
	}
	if _, err := NormalizeTags([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}); err == nil {
		t.Error("超过 8 个应被拒绝")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "a.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	s := NewStore(db)
	ctx := context.Background()
	key := Key{expiry.KindNode, 7}
	if l, err := s.Get(ctx, key); err != nil || l.Region != "" || l.Tags == nil {
		t.Fatalf("没有标签时应返回空标签: %+v %v", l, err)
	}
	if _, err := s.Save(ctx, key, "jp", []string{"IEPL", "东京"}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.All(ctx)
	if l := all[key]; l.Region != "JP" || len(l.Tags) != 2 {
		t.Errorf("all = %+v", all)
	}
	// 两项都清空:那一行删掉。
	if _, err := s.Save(ctx, key, "", nil); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM asset_labels`).Scan(&n)
	if n != 0 {
		t.Errorf("全空时不应留行: %d", n)
	}
}
