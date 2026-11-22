v0.2.2 改进了单题返回 HTTP 400 时的错误说明。上游明确要求更新 Codex 客户端时，检测页会显示这个原因。对于可识别的字段校验错误，页面会说明哪个请求字段缺失、类型有误或不受支持。

失败的题目只发送一次，保持未计分。错误提示使用固定文案，不保存或回显上游错误正文。此版本修复的是错误原因显示，成功归因仍需所选账号发回有效答案。

服务器可运行 `sudo sh /opt/sub2-fingerprint/update.sh` 更新。已有的 Windows 启动器可以继续使用。[安装与维护](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.2.2/deploy/DEPLOYMENT.md)
