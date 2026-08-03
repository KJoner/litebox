// 与后端一致的表单前置校验。
//
// 这些规则后端也会再查一遍 —— 前端这份不是为了安全,是为了让错误在
// 「点提交之前」就说清楚。后置发现的代价太高:创建用户是一次性操作,
// user_code 不可复用,而登录账号没建成时用户拿到的提示是
// 「账号或密码错误」,看起来完全就是密码打错了。

/** 与 portal.ValidateUsername 对应:小写后只允许字母、数字、下划线、连字符与点,3~32 位。 */
const USERNAME_RE = /^[a-z0-9_.-]{3,32}$/

export const USERNAME_HINT = '登录账号只能包含字母、数字、下划线、连字符和点,长度 3~32'
export const PASSWORD_HINT = '密码长度至少 8 位'

/** 校验登录账号。返回 null 表示通过,否则返回给用户看的原因。 */
export function checkLoginUsername(raw: string): string | null {
  // 后端会 TrimSpace + ToLower,这里用同样的归一化再判断,
  // 免得管理员输了大写就被前端拦下、而后端其实接受。
  return USERNAME_RE.test(raw.trim().toLowerCase()) ? null : USERNAME_HINT
}

/** 校验密码。返回 null 表示通过。 */
export function checkPassword(raw: string): string | null {
  return raw.length >= 8 ? null : PASSWORD_HINT
}
