# 构建阶段：编译 wecom-bot
FROM golang:1.25 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/wecom-bot .

# 运行阶段：保留 Go 工具链，首次启动时从挂载的 NekoCode 源码构建 nekocode-tui
FROM golang:1.25
COPY --from=build /out/wecom-bot /usr/local/bin/wecom-bot
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh \
    && mkdir -p /app/workspace /opt/NekoCode
WORKDIR /app
ENTRYPOINT ["/entrypoint.sh"]
