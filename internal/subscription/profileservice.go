package subscription

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/user"
)

// ProfileResult 是一次配置文件订阅请求的产物。
type ProfileResult struct {
	Body        []byte
	ContentType string
	Filename    string
	UserCode    string
	UserInfo    string
	// ProfileName 只进日志,不进响应 —— 内部名称不发给用户。
	ProfileName string
}

// ProfileLink 是门户上的一条配置文件。
//
// 刻意没有的东西:正文、内部名称、内部备注。用户拿它们做不了任何事,
// 而内部备注里往往写着「给谁用的」这种运维话。
type ProfileLink struct {
	ID          int64  `json:"id"`
	Kind        Kind   `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	// Available 为假时 URL 仍然给出来(链接本身没变),但前端只展示原因。
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// ProfileURL 拼一份配置的订阅地址。
//
// 末段带文件名:扩展名影响客户端怎么处理这份响应,摆在 URL 里
// 客户端和管理员都少猜一次。查找按 id,所以改文件名不会断链。
func ProfileURL(baseURL, token string, p Profile) string {
	return fmt.Sprintf("%s/sub/%s/profile/%d/%s", baseURL, token, p.ID, p.Filename)
}

// BuildProfile 按订阅 Token 与模板 ID 生成一份配置文件。
//
// token 是明文;查找走 SHA-256 哈希,与节点订阅同一条路径。
func (s *Service) BuildProfile(
	ctx context.Context, token string, profileID int64, baseURL string,
) (ProfileResult, error) {
	if s.profiles == nil {
		return ProfileResult{}, ErrProfileNotFound
	}
	u, err := s.users.GetBySubTokenHash(ctx, crypto.HashToken(token))
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return ProfileResult{}, ErrNotFound
		}
		return ProfileResult{}, err
	}
	if !u.Serviceable(time.Now().UTC()) {
		return ProfileResult{}, fmt.Errorf("%w:%s", ErrNotServiceable, statusReason(u))
	}

	profile, err := s.profiles.GetProfile(ctx, profileID)
	if err != nil {
		return ProfileResult{}, err
	}
	// 停用的模板等同于不存在:管理员停用它就是要把它从所有人手里撤下来。
	if !profile.Enabled {
		return ProfileResult{}, ErrProfileNotFound
	}

	entries, err := s.buildEntries(ctx, u)
	if err != nil {
		return ProfileResult{}, err
	}

	rendered, err := RenderTemplate(profile.Kind, profile.Content,
		profile.SingBoxLandingDetour, s.profileContext(u, entries, baseURL))
	if err != nil {
		return ProfileResult{}, err
	}

	return ProfileResult{
		Body:        []byte(rendered),
		ContentType: contentTypeFor(profile.Kind),
		Filename:    profile.Filename,
		UserCode:    u.UserCode,
		UserInfo:    userInfoHeader(u),
		ProfileName: profile.Name,
	}, nil
}

func (s *Service) profileContext(u *user.User, entries []Entry, baseURL string) ProfileContext {
	return ProfileContext{
		UserCode: u.UserCode,
		SubURL:   baseURL + "/sub/" + u.SubToken,
		Entries:  entries,
	}
}

// contentTypeFor 给每种配置一个尽量贴切的类型。
//
// 都用 text/plain 也能跑 —— 客户端按内容判断,不看这个头。
// 写准确是为了管理员:他会在浏览器里打开链接核对,
// 而浏览器是唯一一个真的按这个头决定「显示还是下载」的东西。
func contentTypeFor(kind Kind) string {
	switch kind {
	case KindSingBox:
		return "application/json; charset=utf-8"
	case KindClash:
		return "text/yaml; charset=utf-8"
	}
	return "text/plain; charset=utf-8"
}

// PreviewContext 组装某个真实用户的渲染上下文,供管理端预览用。
//
// 预览会把这个用户的 UUID / PSK 显示在管理页上 —— 管理员本来就能在用户
// 详情里看到同样的东西,不额外收紧;但也**不写审计**,它是一次只读渲染,
// 与「查看节点配置差异」同级。
func (s *Service) PreviewContext(
	ctx context.Context, proxyUserID int64, baseURL string,
) (ProfileContext, string, error) {
	u, err := s.users.Get(ctx, proxyUserID)
	if err != nil {
		return ProfileContext{}, "", err
	}
	entries, err := s.buildEntries(ctx, u)
	if err != nil {
		return ProfileContext{}, "", err
	}
	return s.profileContext(u, entries, baseURL), u.UserCode, nil
}

// ProfileLinks 返回该用户可用的全部配置文件。
//
// 每一份都**真的渲染一遍**,只取成功与否。轻量检查(比如数一下落地节点)
// 会与真正的渲染分叉,而分叉的表现是门户上写着「可用」、点进去拿到一句错误 ——
// 那比不显示更糟,因为用户会以为是自己的客户端有问题。
//
// 代价是每次打开订阅页做 N 次字符串替换,N 是模板数(个位数),
// 全部在内存里完成,不碰节点也不碰网络。
func (s *Service) ProfileLinks(
	ctx context.Context, proxyUserID int64, baseURL string,
) ([]ProfileLink, error) {
	// 空切片而不是 nil:nil 序列化成 JSON null,而前端把它当数组用。
	out := make([]ProfileLink, 0)
	if s.profiles == nil {
		return out, nil
	}
	profiles, err := s.profiles.ListProfiles(ctx, true)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return out, nil
	}

	u, err := s.users.Get(ctx, proxyUserID)
	if err != nil {
		return nil, err
	}
	// 不可服务或还没有订阅地址时一份都不给:地址本身就是失效的,
	// 列出来只会变成第二次「你的账号不可用」。
	if !u.Serviceable(time.Now().UTC()) || u.SubToken == "" {
		return out, nil
	}

	entries, err := s.buildEntries(ctx, u)
	if err != nil {
		return nil, err
	}
	ctxData := s.profileContext(u, entries, baseURL)

	for _, p := range profiles {
		full, err := s.profiles.GetProfile(ctx, p.ID)
		if err != nil {
			s.logger.Error("读取配置文件模板失败,已跳过", "profile", p.Name, "error", err)
			continue
		}
		link := ProfileLink{
			ID:          p.ID,
			Kind:        p.Kind,
			Name:        p.PublicName(),
			Description: p.Description,
			Filename:    p.Filename,
			URL:         ProfileURL(baseURL, u.SubToken, p),
			Available:   true,
		}
		if _, err := RenderTemplate(full.Kind, full.Content,
			full.SingBoxLandingDetour, ctxData); err != nil {
			link.Available = false
			link.Reason = renderReason(err)
			// 模板本身的错误(占位符写错)是管理员要处理的,记日志;
			// ErrNotRenderable 是这个用户的节点凑不齐,属于正常情况,不记。
			if !errors.Is(err, ErrNotRenderable) {
				s.logger.Error("配置文件模板渲染失败",
					"profile", p.Name, "user_code", u.UserCode, "error", err)
			}
		}
		out = append(out, link)
	}
	return out, nil
}

// renderReason 把渲染错误变成给用户看的一句话。
//
// ErrNotRenderable 的信息本来就是写给用户的("你的可用节点里没有落地节点"),
// 原样给出去。其余是模板本身的问题,对用户说「配置文件有问题,请联系管理员」——
// 把占位符名字甩给用户,他既看不懂也改不了。
func renderReason(err error) string {
	var nre *NotRenderableError
	if errors.As(err, &nre) {
		return nre.Reason
	}
	return "这份配置暂时无法生成,请联系管理员"
}
