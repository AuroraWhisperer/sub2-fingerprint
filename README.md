# Sub2 Fingerprint

在服务器检测页面选择 Sub2API 中的 OpenAI OAuth 账号，使用 ModelTrace 题目测试本次回答。每题固定请求 `gpt-6-astra` 和 `reasoning.effort=low`。页面显示每个账号的原始答案、有效题数和各候选模型的相对归因概率。

账号列表和状态来自安装者自己的 Sub2API。每次探测先读取账号原始分组，临时加入名称严格为 `Probe` 的分组，重新读取并核对分组后才通过本机 Sub2API 网关发送模型请求。请求结束、失败、超时或取消时恢复原始分组。恢复失败会阻断当前进程后续分组变更。检测器不会安装、启动或控制官方 Codex 客户端。

## 打开与使用

从 [GitHub Releases](https://github.com/AuroraWhisperer/sub2-fingerprint/releases) 下载对应架构的 Linux 包和 `SHA256SUMS.txt`，校验后在私人目录解压。服务器需要 systemd、cgroup v2 和 Python 3。首次安装时提供 Sub2API 地址、Probe 分组 API Key 和分组名称。

```sh
PROBE_API_KEY='在此填入 Probe 分组的 API Key'
printf '%s\n' "$PROBE_API_KEY" | sudo sh ./install.sh --check \
  --sub2-url http://127.0.0.1:8080 --probe-api-key-stdin --probe-group-name Probe
printf '%s\n' "$PROBE_API_KEY" | sudo sh ./install.sh \
  --sub2-url http://127.0.0.1:8080 --probe-api-key-stdin --probe-group-name Probe
```

安装配置固定为 `parallel=1`。每批可以选择多个账号，账号之间依次执行。同一账号的多道题在一次 Probe 分组事务内按账号可用并发容量执行，全部请求结束后才恢复原始分组。Probe Key 写入受权限保护的配置文件，不进入检测结果和日志。

已有旧版本的用户先按[运维说明](deploy/DEPLOYMENT.md)完成迁移。日常更新、恢复和卸载也按该说明执行。

1. Windows 用户从同一发布页下载 `sub2-fingerprint-windows-amd64.zip`，双击 `Sub2-Fingerprint.exe`，启动器会打开服务器检测页 `https://lirahub.dpdns.org/modeltrace/`。自部署可运行 `.\Sub2-Fingerprint.exe -url https://example.com/modeltrace/`，也可把 `-url` 加到快捷方式的目标中。
2. 先在 Sub2API 管理后台登录管理员账号，再打开检测页。检测页会复用同站点的管理员会话，不再要求输入第二组邮箱和密码；原站要求二次验证时，按页面提示完成验证。
3. 选择账号和一至三题，点击“开始检测”。检测器会为每个账号完成一次加组和核对，在账号容量允许的范围内并行发送所选题目，全部题目结束后恢复原始分组。切换账号时，Sub2API 调度缓存可能晚于管理接口更新；遇到 HTTP `503`，检测器会保持当前账号的 Probe 分组关系，按 1、2、4、8 秒间隔重试，最多发送 5 次，且仍受原请求超时和取消控制。成功的题目不会重复发送；重试后仍返回 `503` 才显示 Probe 分组没有可用账号。
4. 检测期间，结果页按账号和挑战显示进度与已完成的原始答案。整批结束后计算归因概率。答案只在检测进程和当前页面内存中使用。

## 真实验收

真实账号验收需要核对以下链路。

- 账号加入了精确名称为 `Probe` 的分组。
- 加组后重新读取的分组核对成功后才发生模型请求。
- 请求模型为 `gpt-6-astra`，推理强度为 `low`。
- 请求完成后账号分组集合恢复为原始集合。
- 账号原本属于 `Probe` 时，该成员关系被保留。
- 无可调度账号时返回的 `503` 被归类为 Probe 分组可用性问题。

## 资源与更新

服务器只在 `127.0.0.1:17861` 监听。访问时由 systemd 启动进程，批次结束且约两分钟没有新请求后退出。工具不保存问答历史，也不创建结果备份。

程序安装在 `/opt/sub2-fingerprint`，检测页不保存 Sub2API refresh token，更新命令如下。

登录管理员后，页面上方的“检查更新”会读取本仓库最新正式发布版。有新版时点击“一键更新”即可下载安装，完成后自动重新连接。更新会重启检测服务并清空内存中的本次结果；检测运行中不能安装更新。下载仍校验 SHA-256，失败时尝试恢复旧版。页面不会自动安装更新。

首次从 v0.2.x 升级时，先在服务器执行下方更新命令，成功后再执行一次：旧脚本第一次只升级程序，第二次由新脚本补齐页面更新服务。全新安装已包含该服务。使用旧 Codex 客户端模式的安装，先按[旧版配置迁移说明](deploy/DEPLOYMENT.md#旧-codex-模式迁移)备份配置，再设置 `parallel=1`、`probe_api_key` 和 `probe_group_name`（`Probe`）。跨模式升级失败时需要恢复旧配置，旧程序不认识新增字段。

```sh
sudo sh /opt/sub2-fingerprint/update.sh --check
sudo sh /opt/sub2-fingerprint/update.sh
```

## 构建与来源

Go 后端只使用标准库，通过本机 Sub2API 网关发送 OpenAI 兼容 Responses 请求。`go test ./...` 运行离线测试，`python3 tools/release.py` 构建 Linux amd64、arm64 安装包及 Windows amd64 启动器。推送与 `VERSION` 相同的版本标签后，GitHub Actions 会测试并发布附带 SHA-256 的安装包。

题目生成器、指纹库和评分代码来自 [ModelTrace](https://github.com/xqy2006/ModelTrace)。固定版本和文件哈希见[来源记录](web/MODELTRACE-PROVENANCE.txt)，许可见 [MIT 许可](web/ModelTrace-LICENSE.txt)。
