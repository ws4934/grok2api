# fingerprint-chromium sherlockToken 铸造 sidecar（Linux / amd64 & arm64 均可构建）
# headful chromium 需要 X server：容器内用 Xvfb :99 提供虚拟显示。
FROM node:22-bookworm-slim

# chromium 运行依赖 + Xvfb + 下载工具；apt 加重试/重试安装，抵御镜像源瞬断
RUN apt-get update -o Acquire::Retries=3 \
    && (apt-get install -y --no-install-recommends \
      xvfb xz-utils curl ca-certificates fonts-liberation \
      libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 \
      libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
      libgbm1 libasound2 libpango-1.0-0 libcairo2 libgtk-3-0 libx11-xcb1 libglib2.0-0 \
      || (sleep 30 && apt-get install -y --no-install-recommends \
      xvfb xz-utils curl ca-certificates fonts-liberation \
      libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 \
      libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
      libgbm1 libasound2 libpango-1.0-0 libcairo2 libgtk-3-0 libx11-xcb1 libglib2.0-0)) \
    && rm -rf /var/lib/apt/lists/*

# 下载 fingerprint-chromium 148（x86_64 Linux 版 141MB；可换 ARG 指向其它版本/镜像）
ARG FPCHROME_URL=https://github.com/adryfish/fingerprint-chromium/releases/download/148.0.7778.215/ungoogled-chromium-148.0.7778.215-1-x86_64_linux.tar.xz
RUN mkdir -p /opt/fpchrome \
    && curl -fsSL "$FPCHROME_URL" | tar -xJ --strip-components=1 -C /opt/fpchrome \
    && test -x /opt/fpchrome/chrome \
    && chmod +x /opt/fpchrome/chrome

WORKDIR /mint
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY mint-service.mjs entrypoint.sh ./
RUN chmod +x entrypoint.sh

ENV MINT_CHROME_BIN=/opt/fpchrome/chrome \
    MINT_PORT=8777 \
    DISPLAY=:99

EXPOSE 8777
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=5 \
  CMD curl -fsS http://127.0.0.1:8777/health >/dev/null || exit 1

ENTRYPOINT ["./entrypoint.sh"]
