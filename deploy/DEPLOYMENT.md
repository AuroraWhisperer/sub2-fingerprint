# 私人账号指纹检测器 · 安装、迁移与卸载

v0.2.0 由 systemd 在服务器的 127.0.0.1:17861 上等待连接。打开检测页才启动 Go 进程。最后一次访问约两分钟后，如果已经没有正在处理的批次，进程会退出，下一次访问会自动启动。每个批次的答案和归因只在本次使用中展示。检测请求会消耗所选上游账号的额度。

## 从 v0.1.0 迁移

先等正在运行的检测结束，关闭检测页。下载对应 CPU 架构的 v0.2.0 正式发布包和同一发布页的 SHA256SUMS.txt，核对整包 SHA-256 后，在只有自己可写的临时目录解压。在解压目录运行以下命令。第一行检查现有安装、端口与资源归属。第二行切换监听方式，验证新版后删除旧版保存在服务器的检测记录。迁移不会生成记录备份。软件发布页位于 [GitHub Releases](https://github.com/AuroraWhisperer/sub2-fingerprint/releases)。

```sh
sudo sh ./install.sh --migrate --check
sudo sh ./install.sh --migrate
```

这一步只在从 v0.1.0 迁移时运行。旧版自带的 update.sh 会在校验新版包结构时停止，原服务仍可使用。新版迁移脚本先检查安装清单、现有进程的可执行文件及新的 socket 和状态目录。发现其他服务使用这些资源时会保留现场。脚本停用旧 service，接手端口，启用 socket 并通过真实 HTTP 请求唤起新版。只有健康接口的版本和进程身份都匹配后才清理旧记录。切换失败会恢复旧程序、原启动状态、原开机设置及旧结果。曾自行单独导出的文件仍由持有人管理。

迁移中途关机或被终止后，从同一版解压目录执行 `sudo sh ./install.sh --migrate --recover`。若已通过健康检查，恢复命令会完成剩余旧结果清理。否则会恢复旧安装。配置及 Sub2API 原站按原方式维护。

## 第一次安装

Linux 主机需要 systemd、cgroup v2 和 Python 3.7 或更高版本。`uname -m` 输出 x86_64 时使用 amd64 包，输出 aarch64 时使用 arm64 包。先从原部署确认 Sub2API 的实际站点地址，在服务器上核对管理 API。示例地址需换成实际地址。

```sh
SUB2_URL='http://127.0.0.1:8080'
curl -sS --max-time 8 -o /dev/null -w 'Sub2API HTTP %{http_code}\n' "$SUB2_URL/api/v1/admin/accounts?page=1&page_size=1&lite=1"
sudo sh ./install.sh --check --sub2-url "$SUB2_URL"
sudo sh ./install.sh --sub2-url "$SUB2_URL"
```

上述未授权的 API 请求应返回 401。外部站点使用 HTTPS。安装会建立专用系统账号、两个 systemd 单元及一个回环监听。程序和配置在 `/opt/sub2-fingerprint`。勾选记住登录后，服务器续期凭据以 0600 权限写入 `/var/lib/sub2-fingerprint`，目录权限为 0700。浏览器持有随机的 HttpOnly 会话标识，最长七天。在页面退出可立即注销该浏览器的会话。正常重启后，勾选过记住登录并且仍获 Sub2API 授权的会话可以恢复。未勾选时需重新登录。

## 打开与验收

在 Windows PowerShell 使用已核验 SSH 主机密钥的个人 SSH 目标。脚本支持 `-SshPort`。Windows 的 TCP 查询功能可用时，脚本只在本机观察转发连接，连续五分钟未见连接就关闭本次 SSH 隧道。再次使用时重新打开脚本。也可直接运行后一条命令，使用完成后在前台按 Ctrl+C 关闭隧道。

```powershell
.\Open-Detector.ps1 -SshTarget my-server
# 或者
ssh -N -T -a -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=yes -L 127.0.0.1:17861:127.0.0.1:17861 my-server
```

访问 `http://127.0.0.1:17861/`，先选一个空闲账号和一道题。测试时可查看 `free -m` 和已有业务响应。并发默认 1，配置文件为 `/opt/sub2-fingerprint/config/config.json`。确有余量时，可将 parallel 改为 2，待当前检测结束，再停下正在运行的 service，下一次访问时生效。512 MB 主机应以实际 MemAvailable、交换和延迟为准。工作进程限制为 MemoryHigh=64M、MemoryMax=96M 和 CPUQuota=20%。

```sh
systemctl is-active sub2-fingerprint.socket
ss -ltn 'sport = :17861'
curl --fail --max-time 8 -sS http://127.0.0.1:17861/healthz
systemctl show sub2-fingerprint.service -p MainPID -p MemoryCurrent
```

健康请求本身会唤醒进程。关闭页面并停止发请求后，约两分钟再用 `systemctl is-active sub2-fingerprint.service` 和 `systemctl is-active sub2-fingerprint.socket` 检查空闲情况，前者应为 inactive，后者应为 active。打开页面仍会重新进入检测。已入队的批次在结束前会延后空闲退出。

## 之后的版本更新

从 v0.2.0 起，日常检查和更新均在安装目录完成。先结束当前检测并关闭页面。更新保留安装配置、仍有效的记住登录和 socket 启用设置。

```sh
sudo sh /opt/sub2-fingerprint/update.sh --check
sudo sh /opt/sub2-fingerprint/update.sh
```

无法连到 GitHub 时，用 `sudo sh /opt/sub2-fingerprint/update.sh --archive /绝对路径/发布包.tar.gz --sha256 该包的64位哈希` 验证并安装本地包。若有未完成的更新，运行 `sudo sh /opt/sub2-fingerprint/update.sh --recover`。用于恢复的软件快照仅包含受管程序和 systemd 文件，成功后清除。检测答案和服务器续期凭据均不进入快照。

## 暂停和卸载

运行下列第一条命令可以立即暂停访问，再用第二条恢复监听。若服务器仍启用了 socket，重新开机后也会自动监听。

```sh
sudo systemctl stop sub2-fingerprint.socket sub2-fingerprint.service
sudo systemctl start sub2-fingerprint.socket
```

彻底移除时，可先在检测页面退出登录，然后执行下面的命令。卸载器会核对清单、UID/GID、单元哈希、目录身份、链接和挂载点，删除检测器自己的程序、socket、专用账号和私有登录状态。遇到归属不符或不认识的文件时会停止，请先检查。完成后重新打开原 Sub2API，核对账号列表和正常业务。

```sh
sudo sh /opt/sub2-fingerprint/uninstall.sh --check
sudo sh /opt/sub2-fingerprint/uninstall.sh
ss -ltn 'sport = :17861'
```
