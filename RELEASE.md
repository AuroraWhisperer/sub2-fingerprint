- 检测页面支持浅色和深色模式。首次打开时跟随系统主题，手动切换后会记住选择。
- 挑战列表默认勾选前三道题，开始检测前仍可调整。

从 v0.2.x 升级时，需先执行命令行升级，成功后再运行同一命令以补齐页面更新服务。全新安装已包含该服务。

修复账号切换时调度缓存尚未同步导致的短暂 503：当前账号保留在 Probe 分组内按 1、2、4、8 秒间隔有限重试，全部题目结束并释放分组后才处理下一账号。

首次安装需要提供 Sub2API 地址、Probe 分组 API Key 和分组名称。日常更新会保留这些配置。

从旧 Codex 客户端模式升级前，须先备份配置，再将 `parallel` 设置为 `1` 并补充 `probe_api_key` 和 `probe_group_name`（`Probe`）。旧程序不接受新增字段，跨模式升级失败时需按运维说明恢复配置。

```sh
PROBE_API_KEY='在此填入 Probe 分组的 API Key'
sudo sh ./install.sh --sub2-url http://127.0.0.1:8080 \
  --probe-api-key "$PROBE_API_KEY" --probe-group-name Probe
sudo sh /opt/sub2-fingerprint/update.sh
```

[安装、更新与卸载](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.3.1/deploy/DEPLOYMENT.md)
