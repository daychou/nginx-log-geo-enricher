# nginx-log-geo-enricher

实时追踪 nginx JSON 访问日志，根据客户端 IP 查询地理位置并追加到每条日志中，输出增强后的 JSON 日志。

## 功能特性

- **实时追踪** — 类 `tail -F` 方式持续追踪日志文件，支持 logrotate 轮转自动切换
- **IP 地理位置查询** — 基于 [ip2region](https://github.com/lionsoul2014/ip2region) 数据库，支持 IPv4 与 IPv6
- **输出同步轮转** — 输入日志按日期轮转（如 `accesslog.log-20260630`）时，输出文件同步轮转同日期后缀
- **轮转文件保留** — 可选按天数自动清理过期的轮转输出文件，避免磁盘占用无限增长（默认不清理）
- **断点续传** — 进程重启后从上次中断位置继续，不丢数据、不重复处理（少量容差）
- **内存可控** — IP 查询缓存上限 100 万条，超限自动清空，防止异常流量导致 OOM
- **优雅退出** — 响应 SIGINT/SIGTERM 信号，先消费完缓冲区数据再退出
- **容错设计** — 单行解析失败写入独立错误日志文件，不阻塞整体流程；flush 失败暂不推进断点
- **错误隔离** — JSON 解析失败的原始行单独记录到 `*_err.log`，避免污染正常日志
- **字段值清洗** — 可选的字段值干扰字符去除（如清洗 `X-Forwarded-For` 中的方括号），避免下游日志平台分词异常

## 快速开始

### 编译

```bash
go build -o nginx-log-geo-enricher main.go
```

### 运行

```bash
./nginx-log-geo-enricher \
  -input  /alidata/logs/nginx/accesslog.log \
  -output /alidata/logs/nginx/accesslog_geo.log \
  -v4db   /data/ip2region_v4.xdb \
  -v6db   /data/ip2region_v6.xdb \
  -field  remote \
  -geofield geo
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-input` | `/alidata/logs/nginx/accesslog.log` | nginx 访问日志文件路径 |
| `-output` | `/alidata/logs/nginx/accesslog_geo.log` | 输出日志文件路径（带地理位置） |
| `-v4db` | ip2region_v4.xdb | IPv4 数据库路径 |
| `-v6db` | ip2region_v6.xdb | IPv6 数据库路径 |
| `-field` | `remote` | JSON 中包含 IP 地址的字段名 |
| `-geofield` | `geo` | 输出 JSON 中存放地理位置的字段名 |
| `-checkpoint` | `<output目录>/.nginx-geo-enricher.checkpoint` | 断点文件路径 |
| `-clean-fields` | (空，不启用) | 需要清洗字符的字段名，逗号分隔（如 `real_ip,x_forwarded_for`） |
| `-clean-chars` | `[]` | 要去除的字符集合 |
| `-retention-days` | `0`（不清理） | 轮转输出文件保留天数，`<=0` 时不清理 |

## 工作原理

```
  accesslog.log ──tail -F──→ 解析 JSON ──→ 提取 IP ──→ ip2region 查询 ──→ 追加 geo 字段 ──→ accesslog_geo.log
                                          │
                                          └── 解析失败 ──→ 记录 _error/_raw ──→ accesslog_geo_err.log
```

1. 以 `tail -F` 模式持续读取输入文件（支持 logrotate 轮转自动切换）
2. 逐行解析 JSON，提取指定 IP 字段
3. 通过 ip2region 查询 IP 对应的省/市/运营商
4. 将地理位置信息追加到 JSON 中，写入输出文件
5. JSON 解析失败的行，记录 `_error`（错误原因）和 `_raw`（原始内容）到错误日志文件
6. 每处理 500 行或 channel 空闲时，推进断点并 flush 输出

### 地理位置字段格式

```
IPv4: 大洲|国家|省|市|区|运营商 → 输出 "省市运营商"
IPv6: 国家|省|市|区|运营商|国家代码 → 输出 "省市运营商"
```

- 内网/回环/链路本地地址 → `内网IP`
- 非法 IP 格式 → `非法IP`
- 查询失败 → `查询失败:<原因>`
- 无 IP 字段 → `无IP字段`

### 输入 JSON 示例

```json
{"remote": "120.229.45.112", "time": "2026-06-30T10:00:00Z", "status": 200}
```

### 输出 JSON 示例

```json
{"geo":"广东深圳移动","remote":"120.229.45.112","status":200,"time":"2026-06-30T10:00:00Z"}
```

### 错误日志格式

JSON 解析失败的行不会丢弃，而是写入与输出文件同目录的 `*_err.log`（如 `accesslog_geo_err.log`），包含错误原因和原始行内容：

```json
{"_error":"JSON 解析失败: invalid character ...","_raw":"原始日志内容"}
```

> 错误日志同样参与断点刷盘和日志轮转，与正常输出文件保持一致的生命周期。

## 字段值清洗

部分字段（如 `http_x_forwarded_for`）可能被恶意传入带方括号的值：

```json
{"real_ip": "[ranip], 36.137.176.36, 118.178.15.113"}
```

上传到 SLS 等日志平台时，方括号会导致分词异常。启用清洗后自动去除指定字符。

### 用法

```bash
# 清洗 real_ip 字段，去除默认的方括号
./nginx-log-geo-enricher -clean-fields real_ip

# 清洗多个字段，自定义去除字符
./nginx-log-geo-enricher -clean-fields "real_ip,http_x_forwarded_for" -clean-chars "[] "
```

### 效果

```
清洗前: {"real_ip": "[ranip], 36.137.176.36", "geo": "广东深圳移动"}
清洗后: {"real_ip": "ranip, 36.137.176.36", "geo": "广东深圳移动"}
```

> **注意**：`-clean-fields` 为空时不启用清洗，零开销，完全向后兼容。清洗仅对字符串类型字段生效，非字符串字段会被跳过。

## 日志轮转行为

### 输入文件轮转

程序通过 inode 检测 logrotate 轮转，检测到后自动切换到新文件从头读取。

### 输出文件同步轮转

当检测到输入文件轮转时，输出文件也会同步轮转：

```
accesslog.log-20260630          (原输入文件被 logrotate 重命名)
accesslog.log                   (新输入文件)

accesslog_geo.log-20260630      (输出文件同步重命名，日期一致)
accesslog_geo.log               (新输出文件)
accesslog_geo_err.log-20260630  (错误日志同步轮转)
accesslog_geo_err.log           (新错误日志文件)
```

若同一天发生多次轮转（极少情况），自动追加序号 `.1`, `.2` 避免覆盖。

### 轮转文件保留清理

通过 `-retention-days` 可自动清理过期的轮转输出文件，避免磁盘占用无限增长：

```bash
# 只保留最近 7 天的轮转输出文件
./nginx-log-geo-enricher -retention-days 7
```

清理机制说明：

- **触发时机**：程序启动时执行一次，之后每次输出文件轮转后再执行一次。
- **清理范围**：仅清理主输出文件和错误输出文件对应的轮转文件（即 `accesslog_geo.log-*` 和 `accesslog_geo_err.log-*`），当前正在写入的文件不受影响。
- **判断依据**：按轮转文件名中的日期后缀（`YYYYMMDD`）判断，保留日期 `>=` `今天 - retention-days + 1` 的文件，更早的删除。文件名不含合法日期后缀的文件会被跳过，不会误删。
- **默认行为**：`-retention-days` 省略或设为 `<=0` 时完全不清理，零开销，向后兼容。

> **注意**：清理基于文件名中的日期而非文件修改时间；由于轮转文件均由本程序按 `YYYYMMDD` 格式生成，格式可控。带日期后缀的压缩历史文件（如 `accesslog_geo.log-20260630.gz`）同样会按日期参与清理。

## 断点恢复机制

程序退出时保存断点（输入文件 inode + 已读偏移量），重启后根据断点状态：

| 情况 | 行为 |
|------|------|
| inode 匹配 | 从上次偏移量继续读取，不丢不重 |
| inode 不匹配（停机期间轮转） | 先通过 inode 找到旧文件读取剩余数据，再从新文件头开始 |
| 旧文件已被删除 | 直接跟踪新文件，可能丢失少量数据 |
| 无断点文件 | 从文件末尾开始，只处理新增数据 |

## 依赖

- Go ≥ 1.26
- [ip2region v2.0](https://github.com/lionsoul2014/ip2region) — xdb 格式 IP 地址库

## License

MIT
