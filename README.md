# log-tools

Linux 日志检索工具，封装了 `grep` 和 `zgrep` 命令，通过 HTTP API 对外提供服务，支持关键词检索普通日志文件（`grep`）和压缩日志文件（`zgrep`）。

---

## 功能特性

- 启动时指定日志目录（`--log-dir`）
- `POST /search` 接收关键词、工具类型、文件模式等参数，拼接并执行原生 Linux 检索命令
- 返回匹配行列表及实际执行的命令（便于排查）
- 支持常用安全标志（`-i`、`-n`、`-v` 等），并通过白名单防止注入
- 防止路径遍历攻击，搜索范围始终限定在指定的日志目录内
- 支持优雅关闭（SIGINT / SIGTERM）
- 可编译为独立的静态二进制文件，无需额外依赖

---

## 编译

```bash
# 编译当前平台二进制
make build

# 也可以直接使用 go build
go build -o log-tools .
```

## 运行

```bash
./log-tools --log-dir /var/log/myapp --addr :9999
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--log-dir` | （必填）| 要检索的日志目录路径 |
| `--addr` | `:9999` | 监听地址 |

---

## API

### GET /health

健康检查。

```json
{ "status": "ok", "log_dir": "/var/log/myapp" }
```

### POST /search

执行日志检索。

**请求体（JSON）：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `keyword` | string | ✅ | 搜索关键词（传递给 grep/zgrep 的 PATTERN） |
| `tool` | string | | `"grep"`（默认）或 `"zgrep"` |
| `file_pattern` | string | | 相对于日志目录的文件名 glob（支持子目录，但不能以 `..` 或 `/` 开头，默认 `*`） |
| `extra_flags` | []string | | 附加安全标志，见下方白名单 |

**允许的 `extra_flags`：**

`-i` / `--ignore-case`、`-n` / `--line-number`、`-c` / `--count`、`-l` / `--files-with-matches`、`-v` / `--invert-match`、`-w` / `--word-regexp`、`-x` / `--line-regexp`

**响应体（JSON）：**

```json
{
  "lines":   ["app.log:2024-01-01 ERROR disk full"],
  "count":   1,
  "command": "grep -r -- ERROR /var/log/myapp"
}
```

发生错误时会返回 `"error"` 字段。

---

## 示例

```bash
# 在所有日志文件中搜索 ERROR
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"ERROR","tool":"grep"}'

# 大小写不敏感搜索，仅限 app.log
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"error","tool":"grep","file_pattern":"app.log","extra_flags":["-i"]}'

# 在压缩日志中搜索（需要 zgrep）
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"TIMEOUT","tool":"zgrep","file_pattern":"*.gz"}'
```

## 测试

```bash
make test
```
