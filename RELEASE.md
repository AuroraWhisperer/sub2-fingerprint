## 更新内容

- 检测页支持按账号名或 ID 搜索、仅看可用账号，也可以在开始检测前更换一组挑战题。
- 检测期间逐题显示进度和已完成的原始答案，整批结束后计算归因概率。连接短暂中断时会自动重试读取进度。
- 修正账号并发容量有限时的批次超时计算。分组恢复会使用同一管理员续期后的会话凭据。恢复失败时，页面会标出受影响的账号并停止批次。
- 检查新版时，检测和状态查询可继续进行。安装完成或回滚后，页面会尝试重连。Linux 安装支持从标准输入传入 Probe API Key，回滚时会清除 systemd 的失败状态。
- Windows 启动器直接打开服务器检测页。自部署用户可以通过 `-url` 指定地址。原来使用 `launcher.local.json` 配置 SSH 的用户，需要改用可访问的检测页地址。

## 升级

现有 Probe 模式安装可在页面点击“检查更新”，或在服务器运行以下命令。更新会保留现有配置。

```sh
sudo sh /opt/sub2-fingerprint/update.sh
```

从 v0.2.x 的 Codex 客户端模式升级时，请先按运维说明备份并迁移配置，再执行命令行更新。成功后再次运行同一命令，补齐页面更新服务。

[安装、更新与卸载](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.4.0/deploy/DEPLOYMENT.md) · [完整改动](https://github.com/AuroraWhisperer/sub2-fingerprint/compare/v0.3.1...v0.4.0)
