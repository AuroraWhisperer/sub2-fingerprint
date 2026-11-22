# Sub2 Fingerprint

在一个独立的私人页面选择 Sub2API 里的 OpenAI OAuth 账号，使用 ModelTrace 的题目测试本次回答。默认每个账号一道题，最多三道。每题固定请求 `gpt-6-astra`、`reasoning.effort=low`，单独发起请求。页面会显示每个账号的原始答案、实际有效题数和库内各候选模型的相对归因概率。

账号列表和状态来自安装者自己的 Sub2API。请求从服务器使用所选账号及其代理发出，推理由上游完成，评分在个人电脑的浏览器中完成。一次检测会消耗所选账号的额度，并与正常使用共享上游限流。检测只携带当前题目，设置 `store=false`。归因描述本次输出的特征，无法用来确认历史对话或独立证明推理强度。

## 打开与使用

从 [GitHub Releases](https://github.com/AuroraWhisperer/sub2-fingerprint/releases) 下载对应架构的 Linux 包和 `SHA256SUMS.txt`，校验后在私人目录解压。服务器需有 systemd、cgroup v2 和 Python 3。首次安装时，将示例中的 Sub2API 地址换成自己的站点地址。

```sh
sudo sh ./install.sh --check --sub2-url http://127.0.0.1:8080
sudo sh ./install.sh --sub2-url http://127.0.0.1:8080
```

已有 v0.1.0 的用户从 **新版解压目录** 运行 `sudo sh ./install.sh --migrate --check`，然后运行 `sudo sh ./install.sh --migrate`。迁移会在确认新版运行正常后清理旧版的检测记录。日常更新、恢复和卸载见[运维说明](deploy/DEPLOYMENT.md)。

1. Windows 用户从同一发布页下载 `sub2-fingerprint-windows-amd64.zip`，解压到希望保留启动器的目录。将 `launcher.example.json` 复制为 `launcher.local.json`，把 `ssh_target` 填为你已使用并核对过的 SSH 配置别名。双击 `Sub2-Fingerprint.exe` 即可在默认浏览器打开 `http://127.0.0.1:17861/`。启动器使用 Windows OpenSSH 建立隧道，空闲约五分钟后自动断开。其他系统可手动建立同样的 SSH 本地端口转发。
2. 登录自己的 Sub2API 管理员账号。勾选“在此浏览器保持登录 7 天”后，服务器在私有目录中保存用于续期的凭据，浏览器持有随机会话标识。管理员密码不会写入文件。原站要求二次验证时，仍需按页面提示验证。
3. 选择账号和一至三题，点击“开始检测”。“同时检测”默认选一个账号，可在服务器设置的上限内选择一、二或三个。每个账号逐题进行，排队的账号在有空位时继续。容量显示 1/20 时可立即测试，20/20 时可选择后等待空位，每五秒重新检查，最多等待五分钟。暂停、限流、凭据过期等状态会显示对应原因。
4. 整批结束后，结果页会按账号和最初选择的挑战显示原始答案，并分别计算每个账号的归因概率。展开条目可以看到排队时分配的检测编号。编号只随本批结果保存在内存中，不进入题目或评分。失败或无效的题目不会计入。若某题出现 HTTP 400，展开该挑战可查看错误来自 Sub2API 或模型上游，以及已识别的原因。答案仅保存在检测进程和当前页面的内存中。下一批成功创建或进程退出时，服务端会释放上一批。已显示的结果仍可在当前页面展开。

## 资源与更新

服务器只在 `127.0.0.1:17861` 监听。访问时由 systemd 启动进程，批次结束且约两分钟没有新请求后退出。检测完成的页面会停止轮询。磁盘上保留安装文件、配置和可选的登录状态。工具不保存问答历史，也不创建结果备份。

程序安装在 `/opt/sub2-fingerprint`，记住登录使用 `/var/lib/sub2-fingerprint`。需要更新时手动运行 `sudo sh /opt/sub2-fingerprint/update.sh --check`，确认后运行 `sudo sh /opt/sub2-fingerprint/update.sh`。Sub2API 可独立更新。升级 Sub2API 后可用一个账号和一道题确认当前接口兼容性。

## 构建与来源

Go 后端只使用标准库。`go test ./...` 运行离线测试，`python3 tools/release.py` 构建 Linux AMD64、ARM64 安装包及 Windows AMD64 原生启动器。推送与 `VERSION` 相同的版本标签后，GitHub Actions 会测试并发布附带 SHA-256 的安装包。

题目生成器、指纹库和评分代码来自 [ModelTrace](https://github.com/xqy2006/ModelTrace)。固定版本和文件哈希见[来源记录](web/MODELTRACE-PROVENANCE.txt)，许可见 [MIT 许可](web/ModelTrace-LICENSE.txt)。
