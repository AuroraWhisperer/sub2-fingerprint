# 私人账号指纹检测器安装与验收

检测器由 systemd 在服务器的 `127.0.0.1:17861` 上按需监听。它通过本机 Sub2API 网关发送探测请求，固定使用 `gpt-6-astra` 和 `reasoning.effort=low`。检测器不会安装、启动或控制官方 Codex 客户端。

每次探测都会先精确查找 `Probe` 分组，读取所选账号的原始 `group_ids`，临时加入该分组并重新读取核对。核对成功后才发送模型请求。探测结束、失败、超时或取消时都会恢复原始分组。恢复失败会阻断当前进程后续的分组变更，直到服务重启并完成检查。

## 第一次安装

Linux 主机需要 systemd、cgroup v2 和 Python 3.7 或更高版本。`uname -m` 输出 `x86_64` 时使用 amd64 包，输出 `aarch64` 时使用 arm64 包。

在服务器上确认 Sub2API 地址和 Probe 分组 API Key。安装命令会把 Key 写入 `/opt/sub2-fingerprint/config/config.json`，文件权限为 `0640`，所有者为 root，组为检测器专用系统组。不要把 Key 写进发布包、源代码或日志。

```sh
SUB2_URL='http://127.0.0.1:8080'
PROBE_API_KEY='在此填入 Probe 分组的 API Key'
curl -sS --max-time 8 -o /dev/null -w 'Sub2API HTTP %{http_code}\n' \
  "$SUB2_URL/api/v1/admin/accounts?page=1&page_size=1&lite=1"
printf '%s\n' "$PROBE_API_KEY" | sudo sh ./install.sh --check \
  --sub2-url "$SUB2_URL" --probe-api-key-stdin --probe-group-name Probe
printf '%s\n' "$PROBE_API_KEY" | sudo sh ./install.sh \
  --sub2-url "$SUB2_URL" --probe-api-key-stdin --probe-group-name Probe
```

上述未授权的管理 API 请求应返回 `401`。外部站点使用 HTTPS。安装会建立专用系统账号、两个 systemd 单元和一个回环监听。程序与配置位于 `/opt/sub2-fingerprint`，检测页会话仅在服务内存和浏览器临时 cookie 中存在。

安装配置的并发值固定为 `1`。每批任务可以选择多个账号，账号之间按单个事务串行执行。同一账号的多道题在一次事务内按账号可用并发容量执行。

## 打开与真实验收

Windows 用户解压本次发布的 `sub2-fingerprint-windows-amd64.zip`，双击 `Sub2-Fingerprint.exe`。启动器会打开服务器检测页 `https://lirahub.dpdns.org/modeltrace/`，不启动官方 Codex 客户端，也不要求本机建立 SSH 隧道。

如需排障或仅在服务器未配置公网反代时使用旧的本地兼容入口，才手动建立隧道：

```sh
ssh -N -T -a -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=yes \
  -L 127.0.0.1:17861:127.0.0.1:17861 my-server
```

先登录 Sub2API 管理后台，再访问 `https://lirahub.dpdns.org/modeltrace/`。检测页会复用同源管理员会话，不会再次显示邮箱密码登录框。真实验收需要核对以下链路。

- 选择的账号原始 `group_ids` 已记录。
- 账号被加入名称严格为 `Probe` 的分组。
- 加组后重新读取的分组集合包含 `Probe`，且模型请求只在核对成功后发生。
- 请求模型为 `gpt-6-astra`，推理强度为 `low`，响应能返回数字答案。
- 请求完成后再次读取账号分组，分组集合与原始集合一致。
- 如果账号原本属于 `Probe`，探测完成后保留这个成员关系。
- 连续检测两个账号时，第二个账号可能在 Sub2API 调度缓存同步前短暂返回 HTTP `503`。检测器在同一次分组事务中按 1、2、4、8 秒间隔重试，最多请求 5 次，等待仍受原请求超时和取消控制；已成功的题目不重发。重试后仍返回 `503` 才显示 Probe 分组暂时没有可用账号，需要检查分组、账号可调度状态和 Sub2API 调度日志。

管理员可以在页面上方点击“检查更新”，再点击“一键更新”安装 GitHub 最新正式发布版。检测进行中会拒绝安装，更新排队或执行期间会拒绝新的检测。更新会清空本页内存中的检测结果。

从 v0.2.x 升级时，先运行 `sudo sh /opt/sub2-fingerprint/update.sh`，成功后再运行同一命令。旧脚本第一次只复制程序与新脚本；第二次由新脚本校验并重新安装同版本发布包，补齐页面更新服务。补齐后，同版本更新会直接返回；全新安装无需这一步。新增的 `sub2-fingerprint-update.path` 会监视固定更新请求，`sub2-fingerprint-update.service` 以独立进程运行已有校验与回滚流程。网页进程仍以普通系统用户运行，不接受自定义更新地址或命令。旧 Codex 模式安装须先在配置中补充 `probe_api_key` 和 `probe_group_name`。

更新失败时可运行 `journalctl -u sub2-fingerprint-update.service -n 80` 查看日志；提示存在未完成更新时，运行 `sudo sh /opt/sub2-fingerprint/update.sh --recover`。失败状态仅显示固定提示，页面不返回服务器日志或配置。

检查服务状态时使用以下命令。

```sh
systemctl is-active sub2-fingerprint.socket
ss -ltn 'sport = :17861'
curl --fail --max-time 8 -sS http://127.0.0.1:17861/healthz
systemctl show sub2-fingerprint.service -p MainPID -p MemoryCurrent
pgrep -af 'sub2-fingerprint|codex-app-server' || true
```

健康请求会唤起检测器进程。停止访问约两分钟后，检测器服务会退出，socket 会继续监听。

## 版本更新

### 旧 Codex 模式迁移

v0.2.x 的默认 `parallel=3` 和配置结构与新模式不同。先等当前检测结束，将配置备份到安装目录之外，再编辑 `/opt/sub2-fingerprint/config/config.json`：设置 `parallel` 为 `1`，添加有效的 `probe_api_key` 和 `probe_group_name`（通常为 `Probe`）。保留其他字段及文件权限。

```sh
sudo cp --no-clobber /opt/sub2-fingerprint/config/config.json /root/sub2-fingerprint-config.before-v0.3.0.json
```

确认备份有效后再编辑配置并立即升级。旧程序不接受新增字段，编辑后不要用旧程序重新启动检测。升级命令成功后再执行一次，以补齐页面更新服务。

此跨模式升级的旧脚本只回滚程序，不会回滚配置。若升级失败并恢复到旧版，先把备份内容写回原配置文件（保留原文件权限），再完成恢复：

```sh
sudo sh -c 'cat /root/sub2-fingerprint-config.before-v0.3.0.json > /opt/sub2-fingerprint/config/config.json'
sudo sh /opt/sub2-fingerprint/update.sh --recover
sudo systemctl reset-failed sub2-fingerprint.service sub2-fingerprint.socket
sudo systemctl start sub2-fingerprint.socket
```

`--recover` 只在存在未完成的更新记录时使用；若自动回滚已经完成，跳过该命令。后续 Probe 模式之间的升级无需更改配置。

### 日常更新

更新会保留当前配置、Probe API Key、Probe 分组名称和 socket 启用状态；ModelTrace 页面会在更新后重新从 Sub2API 当前管理员会话建立临时页面会话。

```sh
sudo sh /opt/sub2-fingerprint/update.sh --check
sudo sh /opt/sub2-fingerprint/update.sh
```

无法联网时，可以使用可信发布包和 SHA-256 校验值进行更新。

```sh
sudo sh /opt/sub2-fingerprint/update.sh \
  --archive /绝对路径/发布包.tar.gz --sha256 该包的64位哈希
```

如果更新中断，先执行恢复命令。

```sh
sudo sh /opt/sub2-fingerprint/update.sh --recover
```

## 暂停与卸载

```sh
sudo systemctl stop sub2-fingerprint.socket sub2-fingerprint.service
sudo systemctl start sub2-fingerprint.socket
```

卸载前先让正在运行的探测结束。卸载器会核对安装清单、专用账号、systemd 单元和配置目录，不会登出 Sub2API。

```sh
sudo sh /opt/sub2-fingerprint/uninstall.sh --check
sudo sh /opt/sub2-fingerprint/uninstall.sh
ss -ltn 'sport = :17861'
```
