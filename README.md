# nginx-log-geo-enricher

实时追踪 nginx JSON 访问日志，根据客户端 IP 查询地理位置并追加到每条日志中，输出增强后的 JSON 日志。

## 功能特性

- **实时追踪** — 类 `tail -F` 方式持续追踪日志文件，支持 logrotate 轮转自动切换
- **IP 地理位置查询** — 基于 [ip2region](https://github.com/lionsoul2014/ip2region) 数据库，支持 IPv4 与 IPv6
- **输出同步轮转** — 输入日志按日期轮转（如 `accesslog.log-20260630`）时，输出文件同步轮转同日期后缀
- **断点续传** — 进程重启后从上次中断位置继续，不丢数据、不重复处理（少量容差）
- **内存可控** — IP 查询缓存上限 100 万条，超限自动清空，防止异常流量导致 OOM
- **优雅退出** — 响应 SIGINT/SIGTERM 信号，先消费完缓冲区数据再退出
- **容错设计** — 单行解析失败输出 fallback 记录，不阻塞整体流程；flush 失败暂不推进断点

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

## 工作原理

```
  accesslog.log ──tail -F──→ 解析 JSON ──→ 提取 IP ──→ ip2region 查询 ──→ 追加 geo 字段 ──→ accesslog_geo.log
```

1. 以 `tail -F` 模式持续读取输入文件（支持 logrotate 轮转自动切换）
2. 逐行解析 JSON，提取指定 IP 字段
3. 通过 ip2region 查询 IP 对应的省/市/运营商
4. 将地理位置信息追加到 JSON 中，写入输出文件
5. 每处理 500 行或 channel 空闲时，推进断点并 flush 输出

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
```

若同一天发生多次轮转（极少情况），自动追加序号 `.1`, `.2` 避免覆盖。

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
