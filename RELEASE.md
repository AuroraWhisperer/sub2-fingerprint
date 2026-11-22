每个账号的每道挑战都通过服务器本机的 Sub2API 网关发送。探测器先把所选账号临时加入 `Probe` 分组，核对分组后请求 `gpt-6-astra`，推理强度固定为 `low`，完成后恢复原始分组。

检测器按账号串行处理分组事务。同一账号的多道题在一次分组事务内按账号可用并发容量执行，模型请求使用 `store=false` 和独立流式响应。失败、取消和超时也会执行分组恢复。

检测页面更新为宽屏双栏布局，检测期间显示批次进度和当前请求状态。

新增管理员“检查更新 / 一键更新”按钮，直接安装本仓库最新正式发布版，保留配置并在完成后重新连接。检测与更新互斥，发布包继续执行哈希校验、健康检查与失败回滚；回滚重连会刷新会话，允许再次更新。从 v0.2.x 升级时，需先执行命令行升级，成功后再运行同一命令以补齐页面更新服务。全新安装已包含该服务。

修复账号切换时调度缓存尚未同步导致的短暂 503：当前账号保留在 Probe 分组内按 1、2、4、8 秒间隔有限重试，全部题目结束并释放分组后才处理下一账号。

首次安装需要提供 Sub2API 地址、Probe 分组 API Key 和分组名称。日常更新会保留这些配置。

从旧 Codex 客户端模式升级前，须先备份配置，再将 `parallel` 设置为 `1` 并补充 `probe_api_key` 和 `probe_group_name`（`Probe`）。旧程序不接受新增字段，跨模式升级失败时需按运维说明恢复配置。

```sh
PROBE_API_KEY='在此填入 Probe 分组的 API Key'
sudo sh ./install.sh --sub2-url http://127.0.0.1:8080 \
  --probe-api-key "$PROBE_API_KEY" --probe-group-name Probe
sudo sh /opt/sub2-fingerprint/update.sh
```

[安装、更新与卸载](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.3.0/deploy/DEPLOYMENT.md)
