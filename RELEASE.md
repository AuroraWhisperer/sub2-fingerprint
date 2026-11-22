v0.2.1 支持最多三个账号同时检测。每批默认同时检测一个账号，可在页面选择两个或三个。同一账号会逐题发送独立请求，超过设定并发数的账号排队等待。

新安装的服务器允许在页面选择 1–3 个并发账号。已有安装使用 `sudo sh /opt/sub2-fingerprint/update.sh` 升级时会沿用原配置，需要开放三并发时将检测器配置中的 `parallel` 设为 `3`，下次进程启动即生效。[安装与运维说明](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.2.1/deploy/DEPLOYMENT.md)
