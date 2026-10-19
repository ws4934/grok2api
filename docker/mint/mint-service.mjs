#!/usr/bin/env node
/**
 * fingerprint-chromium sherlockToken 铸造服务（sidecar）。
 *
 * 职责：管理一个「有头」fingerprint-chromium，按需铸造四字段齐全的 sherlockToken。
 * - GET /health  健康检查（chromium 存在性 + 最近一次铸造状态）
 * - GET /mint    铸造一枚新 token（单飞串行；60s 内有缓存直接复用）
 *
 * 环境变量：
 *   MINT_CHROME_BIN  chromium 二进制路径（默认 /opt/fpchrome/chrome）
 *   MINT_PORT        监听端口（默认 8777）
 *   MINT_HEADLESS=1  切无头（不推荐：无头铸造 token 会被 Adobe 408）
 *   DISPLAY          headful 必须（容器 entrypoint 已用 Xvfb :99 提供）
 *
 * 实测结论（2026-08-23）：
 * - 有头铸造是 200 与否的第一道闸；本服务默认有头。
 * - 铸造页 loads 走直连即可（好 token 不挑铸造 IP），提交链路代理 IP 由应用侧另管。
 */
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import http from "node:http";
import net from "node:net";
import puppeteer from "puppeteer-core";

const CHROME_BIN = process.env.MINT_CHROME_BIN?.trim() || "/opt/fpchrome/chrome";
const PORT = Number(process.env.MINT_PORT ?? 8777);
const HEADLESS = process.env.MINT_HEADLESS === "1";
// 可选出站代理。http(s):// 或无认证 socks5:// 直传给 Chromium；带认证 socks5 经本地中转。
const MINT_PROXY = process.env.MINT_PROXY?.trim() || "";
const MINT_PAGE = "https://firefly.adobe.com/generate/image";
const SHERLOCK_SDK = "https://commerce.adobe.com/amsterdam/sdk/sherlock.min.js";
const CACHE_TTL_MS = 60_000;
const MINT_TIMEOUT_MS = 150_000;

const sleepMs = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

let lastToken = null;
let lastMintAt = 0;
let lastError = "";
let minting = false;
let queued = 0;

function validateFourFields(token) {
  try {
    const parsed = JSON.parse(Buffer.from(token, "base64").toString("utf8"));
    return Boolean(parsed.sid && parsed.ftr && parsed.ark && parsed.bfp);
  } catch {
    return false;
  }
}

async function waitForCdp(port, attempts = 60) {
  for (let i = 0; i < attempts; i++) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (res.ok) return;
    } catch { /* 未就绪 */ }
    await sleepMs(500);
  }
  throw new Error(`chromium CDP ${port} 未就绪`);
}

// byteReader 从 socket 读取恰好 n 字节，用于 SOCKS5 握手。
function byteReader(socket) {
  let buffer = Buffer.alloc(0);
  const waiters = [];
  const pump = () => {
    while (waiters.length && buffer.length >= waiters[0].n) {
      const { n, resolve } = waiters.shift();
      resolve(buffer.subarray(0, n));
      buffer = buffer.subarray(n);
    }
  };
  socket.on("data", (chunk) => { buffer = Buffer.concat([buffer, chunk]); pump(); });
  return (n) => new Promise((resolve, reject) => {
    waiters.push({ n, resolve });
    socket.once("error", reject);
    socket.once("close", () => reject(new Error("socks socket closed")));
    pump();
  });
}

// socks5Connect 向上游 SOCKS5 建立到 destHost:destPort 的隧道（远端 DNS，支持用户名密码认证）。
function socks5Connect(proxy, destHost, destPort) {
  return new Promise((resolve, reject) => {
    const socket = net.connect(proxy.port, proxy.host);
    socket.setNoDelay(true);
    socket.once("error", reject);
    socket.once("connect", async () => {
      try {
        const read = byteReader(socket);
        socket.write(proxy.userId ? Buffer.from([0x05, 0x02, 0x00, 0x02]) : Buffer.from([0x05, 0x01, 0x00]));
        const greet = await read(2);
        if (greet[0] !== 0x05) throw new Error("bad socks version");
        if (greet[1] === 0x02) {
          const u = Buffer.from(proxy.userId ?? "", "utf8");
          const pw = Buffer.from(proxy.password ?? "", "utf8");
          socket.write(Buffer.concat([Buffer.from([0x01, u.length]), u, Buffer.from([pw.length]), pw]));
          if ((await read(2))[1] !== 0x00) throw new Error("socks auth failed");
        } else if (greet[1] !== 0x00) {
          throw new Error("socks no acceptable auth method");
        }
        const hostBuf = Buffer.from(destHost, "utf8");
        socket.write(Buffer.concat([Buffer.from([0x05, 0x01, 0x00, 0x03, hostBuf.length]), hostBuf, Buffer.from([(destPort >> 8) & 0xff, destPort & 0xff])]));
        const head = await read(4);
        if (head[1] !== 0x00) throw new Error("socks connect rejected code " + head[1]);
        const addrLen = head[3] === 0x01 ? 4 : head[3] === 0x04 ? 16 : head[3] === 0x03 ? (await read(1))[0] : 0;
        await read(addrLen + 2); // 丢弃 BND.ADDR + BND.PORT
        socket.removeListener("error", reject);
        resolve(socket);
      } catch (err) {
        socket.destroy();
        reject(err);
      }
    });
  });
}

// startSocksRelay 在 127.0.0.1 起一个无认证 HTTP CONNECT 代理，把隧道转发到带认证的
// 上游 SOCKS5（绕开 Chromium 不支持 socks5://user:pass 的缺陷）。
function startSocksRelay(proxy) {
  return new Promise((resolve) => {
    const server = net.createServer((client) => {
      client.setNoDelay(true);
      let buf = Buffer.alloc(0);
      const onData = (chunk) => {
        buf = Buffer.concat([buf, chunk]);
        const end = buf.indexOf("\r\n\r\n");
        if (end < 0) { if (buf.length > 16384) client.destroy(); return; }
        client.removeListener("data", onData);
        const m = /^CONNECT ([^:\s]+):(\d{1,5})/.exec(buf.subarray(0, end).toString("latin1"));
        if (!m) { client.end("HTTP/1.1 400 Bad Request\r\n\r\n"); return; }
        socks5Connect(proxy, m[1], Number(m[2])).then((up) => {
          client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
          if (buf.length > end + 4) up.write(buf.subarray(end + 4));
          client.pipe(up);
          up.pipe(client);
          const teardown = () => { up.destroy(); client.destroy(); };
          up.on("error", teardown);
          client.on("error", teardown);
          up.on("close", () => client.end());
          client.on("close", () => up.end());
        }).catch(() => client.end("HTTP/1.1 502 Bad Gateway\r\n\r\n"));
      };
      client.on("data", onData);
      client.on("error", () => client.destroy());
    });
    server.listen(0, "127.0.0.1", () => resolve({ port: server.address().port, close: () => new Promise((r) => server.close(() => r())) }));
  });
}

// resolveProxyArg 把 MINT_PROXY 解析成传给 Chromium 的 --proxy-server 参数，必要时起 fp-relay。
async function resolveProxyArg() {
  if (!MINT_PROXY) return { arg: null, close: null };
  let u;
  try { u = new URL(MINT_PROXY); } catch { console.error("[mint] 无效 MINT_PROXY，改走直连"); return { arg: null, close: null }; }
  const scheme = u.protocol.replace(":", "").toLowerCase();
  if (scheme === "http" || scheme === "https") return { arg: `--proxy-server=${MINT_PROXY}`, close: null };
  if (scheme === "socks5" || scheme === "socks5h" || scheme === "socks") {
    if (!u.username) return { arg: `--proxy-server=socks5://${u.host}`, close: null };
    const relay = await startSocksRelay({ host: u.hostname, port: Number(u.port) || 1080, userId: decodeURIComponent(u.username), password: decodeURIComponent(u.password) });
    console.log(`[mint] 认证 SOCKS5 经 fp-relay(127.0.0.1:${relay.port}) 铸造`);
    return { arg: `--proxy-server=http://127.0.0.1:${relay.port}`, close: relay.close };
  }
  console.error(`[mint] 不支持的 MINT_PROXY 协议 ${scheme}，改走直连`);
  return { arg: null, close: null };
}

async function launchChromium() {
  const proxy = await resolveProxyArg();
  const profileDir = await mkdtemp(path.join(tmpdir(), "fp-mint-"));
  const cdpPort = 20000 + Math.floor(Math.random() * 15000);
  const seed = Math.floor(10000 + Math.random() * 90000);
  const args = [
    `--fingerprint=${seed}`,
    "--fingerprint-platform=windows",
    "--fingerprint-brand=Chrome",
    "--no-sandbox",
    "--disable-dev-shm-usage",
    `--remote-debugging-port=${cdpPort}`,
    "--remote-debugging-address=127.0.0.1",
    `--user-data-dir=${profileDir}`,
    "--no-first-run",
    "--no-default-browser-check",
    "--disable-background-networking",
    "--disable-component-update",
    "--disable-sync",
    "--metrics-recording-only",
    "--no-pings",
    "--lang=en-US",
    "--accept-lang=en-US,en;q=0.9",
    "--window-size=1366,768",
  ];
  if (proxy.arg) args.push(proxy.arg);
  if (HEADLESS) args.push("--headless=new");
  const child = spawn(CHROME_BIN, args, { stdio: ["ignore", "ignore", "pipe"] });
  child.stderr.on("data", (chunk) => {
    const text = String(chunk);
    if (/crash|FATAL/i.test(text)) console.error("[fpchrome]", text.trim().slice(0, 300));
  });
  const cleanup = async () => {
    try { child.kill("SIGKILL"); } catch { /* 已退出 */ }
    await sleepMs(300);
    if (proxy.close) await proxy.close().catch(() => undefined);
    await rm(profileDir, { recursive: true, force: true }).catch(() => undefined);
  };
  try {
    await waitForCdp(cdpPort);
  } catch (error) {
    await cleanup();
    throw error;
  }
  const version = await (await fetch(`http://127.0.0.1:${cdpPort}/json/version`)).json();
  if (!version.webSocketDebuggerUrl) {
    await cleanup();
    throw new Error("chromium CDP 未返回 webSocketDebuggerUrl");
  }
  return { ws: version.webSocketDebuggerUrl, cleanup };
}

async function mintOnce() {
  const { ws, cleanup } = await launchChromium();
  try {
    const browser = await puppeteer.connect({ browserWSEndpoint: ws, defaultViewport: null });
    try {
      const page = (await browser.pages())[0] ?? (await browser.newPage());
      await page.goto(MINT_PAGE, { waitUntil: "domcontentloaded", timeout: 60_000 });
      await page.addScriptTag({ url: SHERLOCK_SDK });
      const token = await page.evaluate(`(() => {
        return new Promise((resolve, reject) => {
          let best = null;
          const tryDone = (t) => {
            best = t;
            let d = null;
            try { d = JSON.parse(atob(t)); } catch {}
            if (d && d.sid && d.ftr && d.ark && d.bfp) { clearTimeout(timer); resolve(t); }
          };
          const timer = setTimeout(() => {
            if (best) { try { const d = JSON.parse(atob(best)); if (d && d.ftr && d.ark && d.bfp) { resolve(best); return; } } catch {} }
            reject(new Error("sherlock 铸造超时"));
          }, 90000);
          const sdk = window.SherlockSdk;
          if (!sdk) { clearTimeout(timer); reject(new Error("SherlockSdk 未加载")); return; }
          sdk.initAsync({
            clientId: "clio-playground-web",
            prodEnv: true,
            sessionId: crypto.randomUUID(),
            logging: false,
            tokenCallback: tryDone,
            errorCallback: () => {},
          });
        });
      })()`);
      if (typeof token !== "string" || !validateFourFields(token)) throw new Error("铸造结果四字段不完整");
      // 提交层 UA 对齐（经本服务铸造的 token 配套 UA 记录，供应用侧参考）
      const ua = await page.evaluate(`navigator.userAgent`).catch(() => "");
      console.log(`[mint] ok len=${token.length} ua=${String(ua).slice(0, 60)}`);
      return { token, ua: String(ua) };
    } finally {
      await browser.disconnect();
    }
  } finally {
    await cleanup();
  }
}

/** 单飞串行化：curl 同时在跑多个 mint 时排队复用同一结果；fresh=1 时跳过 60s 缓存强制重铸 */
async function mintWithQueue(fresh = false) {
  if (!fresh && Date.now() - lastMintAt < CACHE_TTL_MS && lastToken) return { token: lastToken, cached: true };
  if (minting) {
    queued += 1;
    for (let i = 0; i < 200 && minting; i++) await sleepMs(500);
    if (!fresh && Date.now() - lastMintAt < CACHE_TTL_MS && lastToken) return { token: lastToken, cached: true };
    // fresh=1 且排队等待结束：取刚铸造出的最新 token（不重复抢铸）
    if (fresh && lastToken) return { token: lastToken, cached: false };
    throw new Error("mint busy timeout");
  }
  minting = true;
  const started = Date.now();
  try {
    let lastFailure = "";
    // 页面加载偶发抖动（TLS reset / SDK 未加载）→ 换全新 profile 窗口重铸 1 次
    for (let attempt = 1; attempt <= 2; attempt++) {
      try {
        const result = await mintOnce();
        lastToken = result.token;
        lastMintAt = Date.now();
        lastError = "";
        console.log(`[mint] done in ${Date.now() - started}ms${attempt > 1 ? ` (attempt ${attempt})` : ""}`);
        return { token: result.token, cached: false };
      } catch (error) {
        lastFailure = error instanceof Error ? error.message : String(error);
        console.warn(`[mint] attempt ${attempt} failed in ${Date.now() - started}ms:`, lastFailure);
      }
    }
    lastError = `2 次尝试均失败: ${lastFailure}`;
    throw new Error(lastError);
  } finally {
    minting = false;
  }
}

const server = http.createServer(async (req, res) => {
  const send = (status, body) => {
    res.writeHead(status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  };
  if (req.method === "GET" && req.url === "/health") {
    const { stat } = await import("node:fs/promises");
    let chromeOk = true;
    try { await stat(CHROME_BIN); } catch { chromeOk = false; }
    send(200, { ok: chromeOk, chrome: CHROME_BIN, headless: HEADLESS, lastMintAt: lastMintAt ? new Date(lastMintAt).toISOString() : null, lastError, busy: minting, queued });
    return;
  }
  if (req.method === "GET" && req.url?.startsWith("/mint")) {
    try {
      const fresh = new URL(req.url, "http://127.0.0.1").searchParams.get("fresh") === "1";
      const result = await Promise.race([mintWithQueue(fresh), sleepMs(MINT_TIMEOUT_MS).then(() => Promise.reject(new Error("mint timeout 150s")))]);
      send(200, { status: "ok", token: result.token, cached: Boolean(result.cached), expires_in_seconds: 1800 });
    } catch (error) {
      send(503, { status: "error", error: error instanceof Error ? error.message : String(error) });
    }
    return;
  }
  send(404, { status: "error", error: "not found" });
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`[mint-service] listening :${PORT} headless=${HEADLESS} chrome=${CHROME_BIN} display=${process.env.DISPLAY ?? "(none)"}`);
});
