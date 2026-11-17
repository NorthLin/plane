# 用 Docker Compose 部署 Plane

这是本仓库完整 Plane 服务端的部署入口，包含网页、管理后台、Space、实时协作、
API、后台任务、数据库、缓存、消息队列和文件存储。CLI 按需安装，不是部署前提。
部署使用已发布镜像，无需安装 Node、Python、Go 或编译源码。

## 首次部署

服务器需安装 Docker Engine 和 Compose v2，支持 Linux amd64 / arm64。
先确认域名、外部访问地址，以及镜像发布者的 Docker Hub 用户名。
这里的用户名是已经发布六个 Plane 镜像的账号，不是部署机器自己的账号。
建议至少 2 核、4 GB 内存，并为数据库和附件预留持久化磁盘空间。

在一个新的目录拉取 main 分支，在仓库根目录运行：

```bash
git clone --branch main https://github.com/NorthLin/plane.git
cd plane
bash configure.sh DOCKERHUB_USERNAME v1.5.0-cli.2 https://plane.example.com
```

将 DOCKERHUB_USERNAME 和访问地址替换为实际值。脚本生成根目录的 .env，权限为
0600，数据库、消息队列、Django 和文件存储分别使用随机凭据；已有 .env 时拒绝覆盖。
不要将 .env 提交到 Git。根目录 .env.example 和 setup.sh 属于开发环境，部署时使用
configure.sh 生成配置即可。

启动前按下面的方式配置 HTTP/HTTPS，然后运行：

```bash
docker compose config --quiet
docker compose pull
docker compose run --rm --no-deps proxy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
docker compose up -d
docker compose logs -f migrator api
```

首次启动会自动迁移数据库，可能需要几分钟。migrator 以退出码 0 结束是正常状态。
浏览器访问 https://plane.example.com/god-mode/，完成实例管理员和实例配置，再进入
主页创建 workspace 和 project。

## 访问地址和 HTTPS

WEB_URL 与 CORS_ALLOWED_ORIGINS 必须填写用户实际访问的完整 origin，包括非标准
端口，不带末尾斜杠。只向外暴露网关，API、数据库和对象存储使用容器内网。

**由自带 Caddy 提供 HTTPS：** 域名解析到服务器，开放 80/443，在 .env 中设置：

```dotenv
WEB_URL=https://plane.example.com
CORS_ALLOWED_ORIGINS=https://plane.example.com
SITE_ADDRESS=plane.example.com
CERT_EMAIL=email admin@example.com
CERT_ACME_CA=https://acme-v02.api.letsencrypt.org/directory
LISTEN_HTTP_PORT=80
LISTEN_HTTPS_PORT=443
```

CERT_EMAIL 的值包含 Caddy 的 email 指令。CERT_ACME_CA 保持生成的默认值，不能留空。

**已有 Nginx、Traefik 等 HTTPS 网关：** 保持 SITE_ADDRESS=:80，代理到本栈 HTTP
端口；例如 LISTEN_HTTP_PORT=8080。外部域名仍配置为 https://plane.example.com。
将 TRUSTED_PROXIES 设置为前置网关的可信 IP/CIDR，NUM_PROXIES 按实际代理层数设置
（自带 Caddy 算一层）。前置网关需支持 WebSocket，并关闭事件流的响应缓冲，读取超时
大于 30 秒。如果 443 被占用，可把 LISTEN_HTTPS_PORT 改到未使用端口。

**先用 HTTP 验证：** 生成配置时传 http://服务器IP:8080，并设置
LISTEN_HTTP_PORT=8080；保持 SITE_ADDRESS=:80。该地址也用于下面的验证命令。

## 验证部署

```bash
docker compose ps -a
curl --max-time 15 -fsS https://plane.example.com/ > /dev/null
curl --max-time 15 -fsS https://plane.example.com/api/instances/
docker compose logs --tail=100 proxy api migrator
```

再在网页创建一个任务、上传并下载图片，确认文件存储可用。如需 agent 接入，
创建 API key 后使用同一个公网地址登录 CLI：

```bash
plane auth login --server https://plane.example.com --workspace WORKSPACE_SLUG
```

无需额外暴露 8000 或 daemon 端口。CLI 用法见 [apps/cli/README.md](apps/cli/README.md)。

CI 使用根目录同一份 Compose 和 configure.sh 启动隔离环境，通过网关验证任务操作、
事件流、本地回调、断线恢复、图片上传下载及 SHA256 一致性。

## 维护和升级

所有服务镜像通过 DOCKERHUB_NAMESPACE 和 APP_RELEASE 统一指定，当前可用版本是
v1.5.0-cli.2。虽然标签包含 cli，它对应完整 Plane 服务端及其事件 API。

- 备份 .env 和所有持久化卷，数据库建议同时保留 pg_dump 备份。文件存储在 uploads 卷。
- 停止服务用 docker compose stop；不要使用 docker compose down -v，它会删除数据卷。
- 升级前备份，再修改 .env 中的 APP_RELEASE，拉取镜像并重新执行迁移：

```bash
docker compose pull
docker compose run --rm migrator
docker compose up -d
```

镜像必须使用同一版本；升级保留原有凭据。数据库迁移后的回滚需要匹配的数据库备份。
移动部署目录或从旧 deployments/release 目录迁移时，先检查原 Compose 项目名和卷名，
使用原来的 docker compose -p PROJECT_NAME，避免误创建空卷。已有实例请复用部署目录和
原 .env，不要重新运行配置脚本生成新凭据。

## 仓库里的 Compose 文件

| 文件                     | 用途                                                                  |
| ------------------------ | --------------------------------------------------------------------- |
| docker-compose.yml       | 完整 Plane 服务端，拉取已发布镜像；部署使用这份                       |
| docker-compose-build.yml | 原来的源码构建栈，按需使用 docker compose -f docker-compose-build.yml |
| docker-compose-local.yml | 本地开发服务，配合 setup.sh 和 pnpm dev                               |
| docker-compose-test.yml  | 后端测试隔离环境                                                      |

deployments/cli/community 中保留的是上游安装器示例，其默认镜像属于 makeplane。
部署本仓库版本请使用根目录入口。
