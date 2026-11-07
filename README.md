# Note-Web

Note-Web 是一个为网络受限/大体积 PDF 设计的轻量化 PDF 阅读器。尤其适合与同时包含 PDF 和同名 .goodnotes 文件的 [GoodNotes](https://www.goodnotes.com/) 备份目录合用。它会递归扫描指定目录中的 PDF 文件，在浏览器中提供文件夹浏览、搜索、分页阅读、缩放、目录跳转和页面标记功能。

如果 PDF 旁边存在同名的 `.goodnotes` 文件，Note-Web 还可以读取 GoodNotes 中保存的目录；对于导入时保留在归档附件中的 PDF 目录，也可以按文档开启深度扫描。

## 安全提示

Note-Web 当前**没有内置用户认证或权限管理**。它适合放在可信内网、VPN 或已经配置认证的反向代理后面；**不要直接把服务端口暴露到不受信任的公网**。文档目录建议以只读方式挂载，数据库与缓存目录单独持久化。

## 功能

- 递归扫描一个或多个文档目录，自动发现新增、修改和删除的 PDF。
- 使用嵌入式前端提供文件库和 PDF 阅读器，无需单独部署前端服务。
- 页面与缩略图按需渲染并缓存；可将常用 PDF 标记为主动缓存，后台预渲染全部页面。
- 被动缓存按 LRU 策略受容量限制，避免缓存无限增长。
- PDF 文件变更后自动失效或重建缓存；主动缓存支持延迟重建，适合仍在同步中的笔记文件。
- 支持 PDF 自带目录、GoodNotes 目录、GoodNotes 归档内嵌 PDF 的深度扫描，以及用户自定义目录和页面标记。
- 支持通过 `server.proxy_path` 在反向代理的子路径下访问。

## 快速开始（Docker）

### 1. 准备目录

将 `docker-compose.yml` 中的两个宿主机路径改成实际路径：

```yaml
volumes:
  - /path/to/your/documents:/documents:ro
  - /path/to/app/data:/data
```

`/documents` 以只读方式挂载，`/data` 用于保存 SQLite 数据库、页面缓存和配置文件。

### 2. 创建配置

复制配置示例到数据目录：

```sh
cp config.yaml.example /path/to/app/data/config.yaml
```

至少确认 `documents.dirs` 与容器内的挂载点一致。示例配置已经使用 `/documents`、`/data/cache` 和 `/data/note-web.db`。

### 3. 启动

```sh
docker compose up -d --build
```

启动后访问 <http://localhost:8080/>。查看日志：

```sh
docker compose logs -f note-web
```

停止服务：

```sh
docker compose down
```

## 本地运行

### 环境要求

- Go 1.25 或更高版本。
- Poppler 工具中的 `pdftoppm`（启动时必需，用于渲染页面）。
- `pdfinfo` 用于读取页数和标题；不可用时会使用 Go 实现读取页数，并以文件名作为标题。
- `mutool` 为可选依赖，用于从没有 `.goodnotes` 伴随归档的 PDF 中提取目录。

在 Debian/Ubuntu 上可使用：

```sh
sudo apt-get install poppler-utils mupdf-tools
```

### 启动

先创建配置文件并按本机路径修改：

```sh
cp config.yaml.example config.yaml
```

然后运行：

```sh
go run . -config config.yaml
```

也可以先构建独立二进制：

```sh
go build -o note-web .
./note-web -config config.yaml
```

`-config` 默认为 `config.yaml`。配置文件不存在时会使用内置默认值，但默认文档目录和数据目录是面向容器的 `/documents` 与 `/data`，本地运行通常应显式提供配置文件。

## 配置

完整配置和每个字段的注释见 [`config.yaml.example`](config.yaml.example)。主要配置段如下：

| 配置 | 作用 | 默认值 |
| --- | --- | --- |
| `server.host` / `server.port` | HTTP 监听地址和端口 | `0.0.0.0` / `8080` |
| `server.proxy_path` | 额外挂载的反向代理子路径 | 空 |
| `documents.dirs` | 要扫描的目录列表 | `[/documents]` |
| `documents.scan_interval` | 定时扫描间隔 | `5m` |
| `documents.retention` | 文件消失后保留记录的时间 | `30d` |
| `documents.max_file_size` | 深度扫描允许读取的 PDF 大小上限 | `1G` |
| `cache.dir` | 页面缓存目录 | `/data/cache` |
| `cache.max_size_mb` | 被动缓存容量上限 | `2048` |
| `cache.active.*` | 主动缓存并发、变更延迟和轮询间隔 | 见示例 |
| `cache.render.*` | JPEG 质量、页面短边渲染宽度和缩略图宽度 | 见示例 |
| `database.path` | SQLite 数据库文件 | `/data/note-web.db` |

时长支持 `s`、`m`、`h`、`d`，例如 `30s`、`1h30m`、`7d`；大小支持 `K`、`M`、`G` 后缀（按 1024 进制计算）。

## 使用说明

1. 将 PDF 放入配置的文档目录。程序启动时会立即扫描，之后按 `documents.scan_interval` 周期扫描。
2. 在文件库中点击文档即可阅读；页面首次访问时会生成 JPEG 缓存，后续访问复用服务端缓存，并通过 ETag 校验内容是否更新。
3. 点击书签图标可将文档设为主动缓存。主动缓存会在后台预渲染全部页面，并在文件变化后按 `cache.active.watch_debounce` 延迟重建。
4. 阅读器中的“目录与标记”面板可用于跳转、添加/删除标记和拖拽排序。批量目录使用“标题 -- 页码”格式，每行一项；省略页码时使用 `0`，表示纯标题。
5. 对带有同名 `.goodnotes` 归档的 PDF，目录优先来自 GoodNotes。需要读取 GoodNotes 归档内嵌 PDF 的原始目录时，在目录面板中开启“目录深度扫描”。

## 数据与目录

- SQLite 数据库保存 PDF 索引、缓存状态、目录和用户标记。
- 缓存目录保存页面 JPEG 和缩略图；删除缓存不会删除原始 PDF。
- 原始 PDF 与 `.goodnotes` 文件始终来自 `documents.dirs`，程序不会修改文档目录中的文件。
- 文件从磁盘消失后会先被软删除；超过 `documents.retention` 后，索引、目录、标记和对应缓存才会被清理。

## 开发与测试

运行全部 Go 测试：

```sh
go test ./...
```

前端资源会通过 Go 的 `embed` 嵌入最终二进制，因此发布时不需要额外携带 `web/` 目录。
