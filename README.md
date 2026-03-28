# log-tools

Linux 日志检索工具，封装了 `grep` 和 `zgrep` 命令，通过 HTTP API 对外提供服务，支持关键词检索普通日志文件（`grep`）和压缩日志文件（`zgrep`）。

---

## 功能特性

- 启动时指定日志目录（`--log-dir`）
- **基于文件名的时间范围检索**（例如，过去1小时或1天）
- **限制返回结果数量**，防止返回过多数据
- `POST /search` 接收关键词、时间范围等参数，拼接并执行原生 Linux 检索命令
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
# 示例：日志文件格式为 myapp-YYYY-MM-DD.log
./log-tools --log-dir /var/log/myapp --addr :9999 --log-name-format 'myapp-%Y-%m-%d.log'
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--log-dir` | （必填）| 要检索的日志目录路径 |
| `--addr` | `:9999` | 监听地址 |
| `--log-name-format` | `""` | 日志文件名的时间格式，用于时间范围检索。支持 `%Y` (年), `%m` (月), `%d` (日), `%H` (时)。 |

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
| `time_range` | string | | 相对时间范围，例如 `"1h"` (1小时), `"30m"` (30分钟), `"1d"` (1天)。如果为空，则搜索所有文件。 |
| `max_lines` | int | | 最大返回行数，对应 `grep -m`。如果为 0 或空，则不限制。 |
| `tool` | string | | `"grep"`（默认）或 `"zgrep"` |
| `extra_flags` | []string | | 附加安全标志，见下方白名单 |

**允许的 `extra_flags`：**

`-i` / `--ignore-case`、`-n` / `--line-number`、`-c` / `--count`、`-l` / `--files-with-matches`、`-v` / `--invert-match`、`-w` / `--word-regexp`、`-x` / `--line-regexp`

**响应体（JSON）：**

```json
{
  "lines":   ["app-2024-03-27.log:2024-03-27 10:00:05 ERROR disk full"],
  "count":   1,
  "command": "grep -r -m 100 -- ERROR /var/log/myapp/app-2024-03-27.log"
}
```

发生错误时会返回 `"error"` 字段。

---

## 示例

```bash
# 在过去一小时的日志中搜索 ERROR，最多返回 100 行
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"ERROR", "time_range": "1h", "max_lines": 100}'

# 在过去一天的日志中大小写不敏感地搜索 "timeout"
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"timeout", "time_range": "1d", "extra_flags": ["-i"]}'

# 在所有压缩日志中搜索 "critical" (需要 zgrep 且文件名匹配时间格式)
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"critical", "tool": "zgrep", "time_range": "7d"}'
```

## 测试

```bash
make test
```
