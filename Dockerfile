# syntax=docker/dockerfile:1.7
# 多阶段构建：golang 编译 → alpine 精简运行镜像。
#
# 运行核本身不常驻，因此镜像的意义只是“随身携带一个 ferc 的干净环境”：
# 既可以在容器里对宿主机做探测与装载，也可以作为 CI 里的发布工具。
#
# 国内网络直连 Docker Hub 常超时，因此基础镜像走 registry mirror 的
# buildx 变量：构建时可用一条命令整体切换镜像站，不必改 Dockerfile。
#   默认（有外网）：    docker build -t ferc .
#   走 1ms.run：        docker build --build-arg MIRROR=docker.1ms.run -t ferc .
# 注意：docker.1ms.run 等站点可能要求登录，匿名拉取会返回 401；此时换
# 一个匿名可用的站点即可（例：--build-arg MIRROR=docker.1panel.live）。
ARG MIRROR=
FROM ${MIRROR}golang:1.27.4-alpine AS build
# 通过 goproxy.cn 拉取依赖, 避免在无外网代理环境(如国内网络/受限内网)构建超时
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ferc ./cmd/ferc

ARG MIRROR=
FROM ${MIRROR}alpine:3.24
# ferc 要调用 systemctl / rc-service / schtasks 等外部命令，并在解包 tar.gz
# 与 zip 后校验产物；镜像里保留 ca-certificates 与 tar 才能完整跑通 bootstrap。
# curl 不装：下载由 ferc 自己的 HTTP 客户端完成。
# adduser -S 默认不建家目录，而 WORKDIR 指向它，因此显式 mkdir。
RUN apk add --no-cache ca-certificates tar \
    && addgroup -S app && adduser -S -G app app \
    && mkdir -p /home/app && chown app:app /home/app
COPY --from=build /out/ferc /usr/local/bin/ferc
# WORKDIR 必须是 app 可写的目录：未指定 --root 时 ferc 会往当前目录写暂存
# 目录；指向 root 所有的 / 会让 dry-run 也失败。
USER app
WORKDIR /home/app
ENTRYPOINT ["ferc"]
CMD ["--help"]
