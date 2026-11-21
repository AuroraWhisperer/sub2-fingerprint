# Sub2 Fingerprint

一个用于 Sub2API 的私人 OpenAI OAuth 账号检测面板。选择服务器上的账号和一至三道 ModelTrace 挑战题，逐账号查看答案和模型归因概率。每题都发起一个新的 Astra 轻度推理请求。多选账号时可排队或双并发，同一账号始终串行。

检测程序通过正常管理员登录和敏感操作验证读取所选账号，使用该账号的有效凭据与代理发送请求。短期凭据停留在进程内存，刷新由 Sub2API 管理。结果保存在本机专属目录，指纹库和评分程序随安装包提供，在浏览器运行。账号显示正常时仍可能遇到上游限流或令牌到期。每次探测都会用到相应账号的上游额度。

## 安装和打开

从 [GitHub Releases](https://github.com/AuroraWhisperer/sub2-fingerprint/releases/latest) 下载适合服务器的 Linux 包和 `SHA256SUMS.txt`，核对所下载文件的 SHA-256 后解压。AMD64 和 ARM64 均有预编译包。安装器使用系统的 Python 3 和 systemd。安装前可从解压目录执行检查。

```sh
sudo sh ./install.sh --check --sub2-url http://127.0.0.1:8080
sudo sh ./install.sh --sub2-url http://127.0.0.1:8080
sudo systemctl enable --now sub2-fingerprint.service
```

把示例地址换成自己的 Sub2API 站点根地址。检测器监听服务器的 `127.0.0.1:17861`。在个人电脑上建立 SSH 本地端口转发，然后访问 `http://127.0.0.1:17861/`，使用自己的 Sub2API 管理员账号登录。

```sh
ssh -N -L 127.0.0.1:17861:127.0.0.1:17861 -o ExitOnForwardFailure=yes YOUR_SSH_ALIAS
```

需要升级时，在服务器执行 `sudo sh /opt/sub2-fingerprint/update.sh --check` 查看最新稳定版，然后执行 `sudo sh /opt/sub2-fingerprint/update.sh` 安装。空闲时更新，原配置和检测记录会保留。恢复和卸载步骤见[运维说明](deploy/DEPLOYMENT.md)。

公开仓库仅包含代码、示例配置、测试样例和固定版本的指纹库。安装者在自己的服务器指定 Sub2API 地址。账号列表和测试答案只在已登录的私人页面展示，评分在本地浏览器完成。

## 检测的范围

- 支持普通持有 access token 的 OpenAI OAuth 账号。测试前重新读取账号状态和当前并发，忙碌、暂停、过期或连接身份无法核对的账号会显示原因。
- 默认测试一道题，最多三道。模型固定请求 `gpt-6-astra`，参数 `reasoning.effort` 为 `low`。每个请求只含当前题，不续接历史对话。
- 使用同一账号本批次的有效回答计算并展示库内各候选的相对概率。无有效回答时显示原因。这个分数描述当次输出特征，不能证明历史对话用过什么模型，也不能独立鉴别推理强度。
- 提交、取消和结果记录按管理员身份隔离。重复提交同一编号返回已有任务，进程重启会标记中断的任务。检测请求直接访问上游，正常业务与它共享账号额度和限流。

## 构建

源码的 Go 后端只依赖标准库。`go test ./...` 运行离线测试，`python3 tools/release.py` 构建发布包。发布时让 `VERSION` 与 Git 标签一致。推送版本标签后，GitHub Actions 测试并制作 Release。安装包带文件哈希和引用许可。

题目生成器、指纹库和评分代码来自 [ModelTrace](https://github.com/xqy2006/ModelTrace)。固定版本、内容校验和及第三方许可位于 [来源记录](web/MODELTRACE-PROVENANCE.txt) 和 [MIT 许可](web/ModelTrace-LICENSE.txt)。
