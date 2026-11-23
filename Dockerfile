# syntax=docker/dockerfile:1.7
# 多阶段构建：golang 编译 → alpine 精简运行镜像。
#
# 运行核本身不常驻，因此镜像的意义只是“随身携带一个 ferc 的干净环境”：
# 既可以在容器里对宿主机做探测与装载，也可以作为 CI 里的发布工具。
FROM golang:1.27.4-alpine AS build
# 通过 goproxy.cn 拉取依赖, 避免在无外网代理环境(如国内网络/受限内网)构建超时
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ferc ./cmd/ferc

FROM alpine:3.24
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
