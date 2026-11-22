每个账号的每道挑战现由服务器按需启动的官方 Codex 客户端发送，沿用所选账号的短期授权和代理。客户端使用新建的临时会话，请求固定为 `gpt-6-astra` 和 `low`。单题至多转发一次，失败可以展开查看原因。

选好账号和题目后即可开始。指纹库在后台下载，补算归因只使用已返回的答案。

服务器先更新检测器，再安装本版本配套的官方客户端。Linux x86-64 需要约 228 MiB 额外磁盘。

```sh
sudo sh /opt/sub2-fingerprint/update.sh
sudo sh /opt/sub2-fingerprint/update.sh --install-runtime
```

[安装、更新与卸载](https://github.com/AuroraWhisperer/sub2-fingerprint/blob/v0.2.3/deploy/DEPLOYMENT.md)
