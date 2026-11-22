v0.2.1 增加了可双击打开的 Windows 原生启动器，并修正账号容量判断、挑战结果和 HTTP 400 的提示。

- Windows 用户解压 `sub2-fingerprint-windows-amd64.zip`，首次设置 `launcher.local.json` 中的 SSH 别名，双击 `Sub2-Fingerprint.exe` 即可打开检测页。已有本地配置可继续使用。
- 每批默认同时测一个账号，最多可选三个。同一账号逐题独立请求。若容量是 1/20，可以直接发题。20/20 时可以勾选，检测器会让其他账号先测，约每五秒查看一次空位，最多等五分钟。
- 批次全部结束后，按账号和最初勾选的挑战展示答案，用随附的 ModelTrace 指纹库分别计算归因。每个账号的每道题在排队时就有自己的临时检测编号，只保存在内存。失败或无效的题目不计入。上游拒绝请求时可在对应挑战下看到失败阶段、诊断代码和可用的上游请求编号。

已有 v0.2.0 安装可通过 `sudo sh /opt/sub2-fingerprint/update.sh` 升级。更新保留原配置，要开放三个并发账号，可在检测器配置中将 `parallel` 设为 `3`，下次进程启动生效。[安装与运维说明](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.2.1/deploy/DEPLOYMENT.md)
