# 私人账号指纹检测器 · 安装与清理

页面由服务器上的 `sub2-fingerprint.service` 提供。程序只监听 `127.0.0.1:17861`。电脑通过已有 SSH 登录访问。每次试测会消耗该账号的上游额度。并发初始值为 1。

## 安装前

在服务器上运行 `uname -m`、`free -m` 和 `systemctl --version`。`x86_64` 选 amd64 包，`aarch64` 选 arm64 包。需要 Python 3.7+ 执行安装及卸载，运行检测器只需要包里的 Go 程序。主机应使用 systemd 和 cgroup v2。先记录原站正常业务的响应状况和可用内存。512 MB 主机先观察高峰的 `MemAvailable`，1 GB 是部署报告建议的容量。

从原部署确认 Sub2API 实际站点根地址，保留原 HTTPS 证书检查。本机回环的 HTTP 地址也可使用。可用管理员账号列表的**未授权**请求核对地址和访问路径。符合预期时应收到 HTTP 401，先处理 404、502 或登录页 HTML。操作过程中无需复制任何账号导出文件。

```sh
# 替换为在原部署中确认的地址，以下示例端口没有固定要求。
SUB2_URL='http://127.0.0.1:8080'
curl -sS --max-time 8 -o /dev/null -w 'Sub2API HTTP %{http_code}\n' "$SUB2_URL/api/v1/admin/accounts?page=1&page_size=1&lite=1"
```

核对从受信交付位置取得的**整包 SHA-256**。在只有自己能写入的临时目录解压，然后进入含 `install.sh` 的目录。预检验证包内文件哈希、架构、控制组、账号名称、安装目录和监听端口。它只读取状态并短暂尝试绑定空闲回环端口。

```sh
sudo sh ./install.sh --check --sub2-url "$SUB2_URL"
sudo sh ./install.sh --sub2-url "$SUB2_URL"
```

安装发现同名目录、服务或系统用户就会停止。安装完成时服务仍为停止状态。程序位于 `/opt/sub2-fingerprint/bin/`，非敏感配置位于 `config/config.json`，检测结果在 `data/runs/`。服务账户只能写该结果目录。卸载依据 `install-manifest.json` 中的资源清单和目录身份。

## 试运行与日常访问

安排一个正常业务空闲时段。服务上限为 `MemoryHigh=64M`、`MemoryMax=96M` 和 `CPUQuota=20%`。这些数值是限制，第一次实测后才能知道峰值。低内存机器先保持并发 1。若余量很少、出现持续换页或已有 OOM，先保留已安装且暂停的服务。

```sh
sudo systemctl start sub2-fingerprint.service
systemctl show sub2-fingerprint.service -p MainPID -p MemoryCurrent -p MemoryHigh -p MemoryMax -p CPUQuotaPerSecUSec
curl --fail --max-time 5 -sS -o /dev/null -w '检测器 HTTP %{http_code}\n' http://127.0.0.1:17861/healthz
ss -ltn 'sport = :17861'
```

健康检查须返回 200，监听地址须为 `127.0.0.1:17861`。在 Windows 电脑的 PowerShell 中，使用现有已核验主机密钥的 SSH 目标运行以下命令。将 `my-server` 换成你确认的 SSH 别名。

```powershell
.\Open-Detector.ps1 -SshTarget my-server
# 也可直接用 OpenSSH
ssh -N -T -a -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=yes -L 127.0.0.1:17861:127.0.0.1:17861 my-server
```

打开 `http://127.0.0.1:17861/`，在检测页面完成管理员登录以及实际启用的二次验证。若服务器 SSH 使用非默认端口，可为脚本加 `-SshPort 端口号`。Ctrl+C 结束本次前台隧道。此前从未连接过此 SSH 主机时，先通过服务商控制台或其他独立渠道核对主机密钥指纹，按平时的 SSH 登录步骤记录已核实的主机密钥。

1. 先选一个正常且空闲的普通 OpenAI OAuth 账号及一道题。检查账号 ID、时间、上游回应和有效答案数。所有题目均失败时页面应说明原因。
2. 测试前后观察 `free -m`、正常业务的响应和服务内存。遇到 401 先区分后台会话、二次验证和上游账号凭据。凭据过期交给 Sub2API 刷新后再手工重试。代理、账号身份或模型档位不支持时，保留该账号的失败状态。
3. 单账号验证通过且内存和业务延迟允许后，把 `config/config.json` 的 `parallel` 从 1 改为 2，再仅重启本服务。选择两个空闲账号，看请求时间、答案和归因是否正确归属。每个账号自己的题目顺序运行。
4. 需要开机运行时再使用 `sudo systemctl enable sub2-fingerprint.service`。平时只偶尔测试，使用前 `start`、用完 `stop` 即可。浏览器刷新仅查询进度，服务中断后要新测由你手工选择。

若账号的代理只在应用容器网络内可达，先核对其实际网络可达性和本机管理接口地址。服务只拥有运行业务请求所需权限。测试失败时查看 `journalctl -u sub2-fingerprint.service --since '10 min ago' --no-pager`。分享诊断时可提供错误类别、时间及版本，敏感信息留在本机。

## 手动更新检测器

稳定版发布在 [GitHub Releases](https://github.com/AuroraWhisperer/sub2-fingerprint/releases)。安装目录自带更新脚本，按需运行。先等本次检测结束，再在 SSH 终端执行以下命令。第一条仅查询正式版本，第二条在有新版本时更新。

```sh
sudo sh /opt/sub2-fingerprint/update.sh --check
sudo sh /opt/sub2-fingerprint/update.sh
```

脚本按 CPU 架构选择发布包，核对整包 SHA-256、包内文件和版本。更新保留本机配置、检测记录和开机启动设置。原服务处于运行状态时会短暂重启并核对进程和健康接口报告的版本。重启后需在检测页重新登录。原服务已停止时会保持停止。使用 Sub2API 自己的更新方式照常更新原站。在重大版本变更后可先选一个空闲账号和一道题验证兼容性。

服务器无法访问 GitHub 时，可从同一正式发布页下载对应架构的包和 `SHA256SUMS.txt`，核对并把包上传到服务器。运行 `sudo sh /opt/sub2-fingerprint/update.sh --archive /绝对路径/发布包.tar.gz --sha256 该包对应的64位哈希`。

运行中的新版未通过健康检查时，脚本自动恢复旧程序。更新过程中断电或 SSH 进程中断后，若提示存在未完成的更新，执行 `sudo sh /opt/sub2-fingerprint/update.sh --recover`，然后检查本服务的状态。启动状态以更新前为准。测试中的请求如已发往上游，可能已经记入额度，需由你决定是否手工重试。

## 停用与清理

发生影响时先在检测页退出自己的登录会话，并停止本服务。有紧急资源压力时可直接停止。原账号管理和已有对话仍使用原站。

```sh
sudo systemctl stop sub2-fingerprint.service
sudo sh /opt/sub2-fingerprint/uninstall.sh --check
```

只在确认整套检测器需要移除时执行卸载。下面的命令会将检测结果备份到**新建的**指定文件，再移除本工具。选择安装目录外只有自己可访问的路径，备份为 `0600` 权限。

```sh
sudo sh /opt/sub2-fingerprint/uninstall.sh --backup-to /root/sub2-fingerprint-runs.tar.gz
```

没有检测结果时可直接执行 `sudo sh /opt/sub2-fingerprint/uninstall.sh`。明确需要删除已有结果时可用 `--purge-results`。卸载程序会核对安装清单、专用用户、服务文件、链接及挂载。额外文件或被替换的服务文件需要先检查归属。运行方自行管理的备份、系统日志和原站审计记录遵循各自保留期限。

```sh
systemctl is-active sub2-fingerprint.service
ss -ltn 'sport = :17861'
test ! -e /opt/sub2-fingerprint
```

预期服务状态为 `inactive` 或找不到该单元，端口无监听，专属目录消失。最后用原方法打开 Sub2API 查看账号列表并确认正常业务。电脑端关闭隧道，按需保留或移除快捷脚本及下载的安装包。
