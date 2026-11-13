# jmstool

用于排查 JumpServer / Koko 和目标资产连接问题的 Go 命令行工具。

| 命令 | 用途 |
| --- | --- |
| `ssh` | 交互式 SSH Shell、执行单条命令、验证认证与算法协商 |
| `sftp` | 下载文件，验证 SSH 认证、SFTP 子系统和实际传输 |
| `telnet` | 自动登录、提示符识别、终端协商与交互调试 |

工具直接连接指定服务，不依赖 Core API。默认端口为 SSH/SFTP `22`、Telnet `23`；连接 Koko 时通常需要显式传入 `-p 2222`。

## 构建与运行

需要 **Go 1.26 或更新版本**。

```bash
git clone https://github.com/LeeEirc/jmstool.git
cd jmstool
go build -o jmstool .
./jmstool --help
./jmstool --version
```

Windows 可以使用 `go build -o jmstool.exe .`。发布包包含可执行文件、本说明、许可证和可选的 SSH 算法配置。

## SSH / SFTP 认证与主机密钥

SSH 和 SFTP 共用连接参数：

| 参数 | 默认值 / 行为 |
| --- | --- |
| `-p, --port` | `22` |
| `-i, --identity` | 指定私钥文件，支持加密私钥和 `~/` 路径 |
| `-P, --password` | 显式指定密码，空字符串也是有效的显式输入 |
| `--known-hosts` | `~/.ssh/known_hosts` |
| `--insecure` | 仅在显式指定时跳过服务器主机密钥校验 |
| `--timeout` | `30s`，覆盖 TCP 连接和 SSH 握手 |
| `-c, --config` | 可选的 SSH 算法 YAML 文件 |

默认校验 `known_hosts`。第一次连接时，应通过可信渠道核对主机指纹，再建立相应记录；也可以使用已经核验指纹的 OpenSSH 连接建立记录。自定义端口的条目需要使用 `[host]:port` 格式。未知或变化的密钥会使连接失败，错误信息包含收到的指纹。程序不会自动信任新密钥，也不会修改已有记录。

服务器提供多个主机密钥时，未显式设置 `HostKeyAlgos` 的连接会优先协商 `known_hosts` 中已有密钥的算法。

密码优先级为：显式 `-P` → `JMSTOOL_PASSWORD` → 终端隐藏输入。显式的 `-P ''` 不会被环境变量覆盖。命令行参数可能被进程列表或 Shell 历史记录保存，人工使用时建议直接省略 `-P`，自动化环境通过密钥或受控的环境变量提供密码。

加密私钥可通过 `JMSTOOL_KEY_PASSPHRASE` 提供口令，未提供时使用隐藏输入。支持 SSH keyboard-interactive 认证；验证码等交互问题会单独询问，不会直接填入已保存的账户密码。没有终端且缺少必要凭据时，会返回明确错误。

握手期间的密码或 MFA 输入也会占用 `--timeout` 的预算；需要较长等待时可以使用 `--timeout 2m`。

## SSH

```bash
# 连接 Koko，按提示输入密码
jmstool ssh admin@jumpserver.example.com -p 2222

# 私钥登录
jmstool ssh root@asset.example.com -i ~/.ssh/id_ed25519

# 单条远程命令；默认不申请 PTY，保留 stdout、stderr 和远程退出码
jmstool ssh root@asset.example.com --command 'uname -a'

# 管道输入原样交给远端 Shell
printf 'uname -a\nexit\n' | jmstool ssh root@asset.example.com -T

# 仅用于明确允许跳过主机校验的诊断环境
jmstool ssh root@192.0.2.10 --insecure
```

交互式 Shell 在本地输入是终端时自动申请 PTY，终端类型取 `TERM`，其次兼容旧版 `xterm` 环境变量，最后使用 `xterm-256color`。`-t/--pty` 强制申请，`-T/--no-pty` 禁用，两者互斥。无法获取尺寸时使用 80 列、24 行。

目标地址在最后一个 `@` 处分割，因此可以使用包含 `@` 的账户名或 JumpServer 选择器。IPv6 可写为 `user@[2001:db8::10]`；端口统一使用 `-p`。

Unix 下窗口尺寸变化会同步到远端。Windows 当前不支持窗口变化监听；初始 PTY 尺寸仍会发送。Shell 启动失败、断线和取消时均执行本地终端恢复。

## SFTP 下载

```bash
# 默认保存为当前目录下的 file.txt
jmstool sftp admin@jumpserver.example.com -p 2222 -d /tmp/file.txt

# 指定本地文件名
jmstool sftp root@asset.example.com -d /var/log/app.log -o ./app-copy.log

# 指定已有目录
jmstool sftp root@asset.example.com -d /var/log/app.log -o ./downloads/

# 明确允许在完整下载后替换同名文件
jmstool sftp root@asset.example.com -d /var/log/app.log -o ./app-copy.log --force
```

`--download/-d` 必填；`--output/-o` 接受本地文件路径或已有目录，本地父目录必须存在。默认拒绝覆盖已有文件，包括传输过程中由其他进程创建的同名文件。

下载先写入同目录临时文件，远端读取和关闭、本地同步和关闭均成功后才发布。下载失败会清理临时文件并保留原文件。Unix 下临时文件以 `0600` 创建；Windows 上访问权限由目标目录 ACL 等系统规则决定。默认发布使用硬链接防止覆盖竞争，因此目标文件系统需要支持硬链接；在 FAT/exFAT 等文件系统上，可以在确认允许替换后显式使用 `--force`，改用重命名发布。重命名的原子性由平台和文件系统决定。

当前命令只提供单文件下载，保留顺序读取方式，适合连接和传输诊断。

## Telnet

使用 [tclientlib v0.1.2](https://github.com/LeeEirc/tclientlib/releases/tag/v0.1.2)。Telnet 会以明文传输凭据和会话内容，应在适当的网络环境中使用。

```bash
# 自动登录，按提示输入密码
jmstool telnet admin@192.0.2.10

# 自定义最终 Shell 提示符；内置登录和 Shell 提示符仍然有效
jmstool telnet admin@192.0.2.10 -c '^READY>$'

# 兼容旧设备的 CR-NUL 登录结束符
jmstool telnet admin@192.0.2.10 --line-ending crnul

# 服务器不出现密码提示时，仅使用用户名登录
jmstool telnet admin@192.0.2.10 -P ''

# 显式开启协议追踪，输出到 stderr
jmstool telnet admin@192.0.2.10 --debug
```

| 参数 | 默认值 / 行为 |
| --- | --- |
| `-p, --port` | `23` |
| `-P, --password` | 与 SSH 相同的密码优先级 |
| `-c, --custom` | 补充最终 Shell 提示符正则表达式 |
| `--timeout` | `30s`，TCP 连接和初始登录共用预算 |
| `--line-ending` | `crlf`，也可选 `crnul`、`cr`、`lf` |
| `--debug` | 默认关闭；开启后将按连接输出协议追踪 |

交互终端的 Enter 会转换为指定结束符；管道或文件输入保留原始字节，由调用方提供需要的换行。标准输入结束后，客户端会发送 TCP 写半关闭并继续接收输出，直到服务器关闭连接；仍可用 Ctrl-C 取消。

Telnet 的 `-P ''` 会阻止环境变量和本地密码提示填入密码，仅适用于服务器不要求密码的登录。tclientlib 遇到密码提示且密码为空时会拒绝登录，不会向服务器发送空密码认证。

无效正则以及可匹配空字符串的正则会在连接前被拒绝。`--custom` 是补充规则，不会替换内置规则。协议追踪会隐藏自动登录时的凭据，但登录后的会话数据仍可能包含敏感内容。

## SSH 算法配置

`ssh` 和 `sftp` 的 `-c/--config` 共用以下 YAML 格式：

```yaml
Ciphers: [aes128-gcm@openssh.com, aes256-gcm@openssh.com]
KexAlgos: [curve25519-sha256]
MACs: []
HostKeyAlgos: [ssh-ed25519]
```

空列表或省略字段使用库默认值。未知字段、无效 YAML、多文档 YAML 及不支持的算法会报错，避免拼写错误被静默忽略。

随附的 `ssh_config.yml` 保留了用于旧设备排障的兼容算法，包括一些弱算法；只有明确传入 `-c ssh_config.yml` 时才生效。正常连接建议省略 `-c`。示例中原来的 `urve25519-sha256` 拼写错误已修正。

## 从旧版本升级

- **Telnet**：依赖由 `v0.0.2` 更新到 `v0.1.2`，迁移到 `ClientConfig` 和每连接 `Trace`；`WindowChange` 使用新版的“高度、宽度”顺序。登录结束符默认改为 CRLF，需要旧行为时使用 `--line-ending crnul`。详见上游[迁移说明](https://github.com/LeeEirc/tclientlib/blob/v0.1.2/CHANGELOG.md)。
- **SSH 主机校验**：从始终跳过校验改为默认使用 `known_hosts`；诊断环境仍可显式使用 `--insecure`。
- **SSH 依赖与构建**：`golang.org/x/crypto` 更新到 `v0.59.0`，包含客户端断线与通道处理修复，因此 Go 最低版本提高到 `1.26`。上游已移除 DSA/`ssh-dss` 支持，DSA 私钥和主机密钥不再可用；RSA、ECDSA、Ed25519 仍可使用。
- **配置**：删除未实际参与连接配置的 Viper 初始化和 `--toggle` 模板参数，不再自动读取 `~/.jmstool.yaml`。算法配置请通过 `ssh/sftp -c` 显式指定。
- **退出状态**：运行错误返回非零状态；SSH 远程命令的有效退出码会保留。SFTP 失败不再仅打印日志后返回成功。
- **下载覆盖**：已有本地文件需要显式指定 `--force`。

## 开发与发布

```bash
go test -race ./... -timeout 2m
go vet ./...
make all VERSION=dev
```

`make all` 生成 Linux、macOS 和 Windows 的 amd64/arm64 共六个发布包；可单独使用 `make linux-amd64` 等目标。通过 Makefile 构建时，`--version` 包含版本、提交 SHA 和构建时间。普通 `go build` 使用开发版占位信息。

测试使用本地 SSH、SFTP 和 Telnet 测试服务，不需要真实的 JumpServer 账户。覆盖认证、主机密钥、超时和取消、stdout/stderr、远程退出码、传输失败及文件覆盖竞争、Telnet IAC/NAWS 协商和迁移兼容性；Linux 下额外使用伪终端验证终端恢复。

持续集成在 Linux、macOS、Windows 上执行测试，并交叉编译发布目标。推送 `v*` 标签时，先验证和构建，再生成草稿 Release、上传发布包与 SHA-256 校验文件。
