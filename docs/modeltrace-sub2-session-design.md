# ModelTrace Sub2API 会话桥接

ModelTrace 页面运行在与 Sub2API 相同的 HTTPS 站点下。Sub2API 前端把当前管理员 access token 存在同源 `localStorage` 的 `auth_token` 键中；ModelTrace 页面读取该键，并只把 access token 放入同源 API 请求的 `Authorization: Bearer` 请求头。

ModelTrace 后端收到 token 后调用 Sub2API `/auth/me`，确认用户仍是 active administrator，再创建仅存在于内存和临时浏览器 cookie 中的页面会话。后端不接收、保存或轮换 `refresh_token`，也不把 Sub2API token 写入日志或返回给页面。页面退出只清理 ModelTrace 会话，不调用 Sub2API `/auth/logout`。

页面没有邮箱、密码或登录验证码表单。没有有效 Sub2API 管理员会话时，只显示返回管理后台的链接。access token 在 Sub2API 前端刷新后，ModelTrace 下一次请求会重新核验新的 token。

验证范围：

- access token 必须能通过 `/auth/me`，且角色为 `admin`、状态为 `active`。
- 账号、题目、提交和结果接口仍由 ModelTrace 页面会话保护，并保留 CSRF 和同源 Origin 检查。
- ModelTrace 页面会话不持久化 refresh token，不影响 Sub2API 更新、刷新或登出状态。
