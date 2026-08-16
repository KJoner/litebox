package subscription

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
)

func newProfileStore(t *testing.T) *ProfileStore {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "profiles.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	return NewProfileStore(db)
}

// 文件名留空,让它回落到建议值 —— 内部名称允许中文,文件名不允许。
func srParams(name string) ProfileParams {
	return ProfileParams{
		Kind:    KindShadowrocket,
		Name:    name,
		Content: "[General]\nbypass-system = true\n",
		Enabled: true,
	}
}

func TestProfileCRUD(t *testing.T) {
	s := newProfileStore(t)
	ctx := t.Context()

	p, err := s.CreateProfile(ctx, srParams("小火箭默认"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Content == "" || p.ContentBytes == 0 {
		t.Fatal("详情里应当有正文与字节数")
	}

	// 列表**不带正文**:十份模板就是几百 KB 跟着每一次刷新走,
	// 而列表页一个字都不显示。
	list, err := s.ListProfiles(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("列表条数 = %d", len(list))
	}
	if list[0].Content != "" {
		t.Error("列表里不该带正文")
	}
	if list[0].ContentBytes != p.ContentBytes {
		t.Error("列表里仍然要能看出这份有多大")
	}

	updated, err := s.UpdateProfile(ctx, p.ID, ProfileParams{
		Kind: KindShadowrocket, Name: "小火箭默认", Filename: "x.conf",
		Content: "[General]\nnew = 1\n", Enabled: true, SortOrder: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Filename != "x.conf" || updated.SortOrder != 5 {
		t.Errorf("更新没生效:%+v", updated)
	}

	if err := s.SetProfileEnabled(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.ListProfiles(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 0 {
		t.Error("停用的模板仍然出现在启用列表里")
	}
}

// 软删除 + AUTOINCREMENT:id 永远不会被复用。
// 复用的话,用户手上的旧链接某天会指向一份全新的配置 ——
// 那会在他毫不知情的情况下换掉整台机器的网络栈行为。
func TestProfileIDIsNeverReused(t *testing.T) {
	s := newProfileStore(t)
	ctx := t.Context()

	first, err := s.CreateProfile(ctx, srParams("甲"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProfile(ctx, first.ID); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("删除后仍能读到:%v", err)
	}

	second, err := s.CreateProfile(ctx, srParams("乙"))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("新模板拿到了已删除模板的 id %d", second.ID)
	}
}

// 软删除之后名字可以复用,与外部代理一致。
func TestProfileNameConflict(t *testing.T) {
	s := newProfileStore(t)
	ctx := t.Context()

	p, err := s.CreateProfile(ctx, srParams("同名"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateProfile(ctx, srParams("同名"))
	if err == nil || !strings.Contains(err.Error(), "同名") {
		t.Fatalf("重名没有被拦下来:%v", err)
	}

	if err := s.DeleteProfile(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProfile(ctx, srParams("同名")); err != nil {
		t.Errorf("软删除之后名字应当可以复用:%v", err)
	}
}

// 存进去的校验与 ValidateTemplate 是同一条路径 ——
// 绕过它写库的话,坏模板要等到用户拉取时才暴露。
func TestProfileCreateRunsTemplateValidation(t *testing.T) {
	s := newProfileStore(t)
	_, err := s.CreateProfile(t.Context(), ProfileParams{
		Kind: KindClash, Name: "机场直连", Filename: "c.yaml",
		Content: "proxy-providers:\n  p:\n    url: https://my.example.com/sub?token=SECRET\n",
	})
	if err == nil {
		t.Fatal("没有占位符的 Clash 模板被写进库了")
	}
}

// 非 sing-box 的模板不保留落地前置出站 —— 留着它只会让人以为那里配了什么。
func TestLandingDetourClearedForOtherKinds(t *testing.T) {
	p := ProfileParams{
		Kind: KindShadowrocket, Name: "x", Content: "[General]\n",
		SingBoxLandingDetour: "前置节点",
	}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.SingBoxLandingDetour != "" {
		t.Error("非 sing-box 模板的落地前置出站没有被清掉")
	}
}
