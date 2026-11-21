v0.2.0 支持按需启动、空闲退出和可选的七天登录。当前批次的答案只保存在内存中，结果页完成后停止轮询。每道题单独请求，评分使用随包提供的 ModelTrace 指纹库。

已有 v0.1.0 的用户请下载并校验对应架构的新版包，从解压目录运行 `sudo sh ./install.sh --migrate --check` 和 `sudo sh ./install.sh --migrate`。确认新版健康后，安装器会清理旧检测记录。后续更新和无备份卸载见 [运维说明](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.2.0/deploy/DEPLOYMENT.md)。
