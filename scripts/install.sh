#!/bin/sh
# Install or upgrade grok2api with Docker Compose.
#
#   curl -fsSL https://raw.githubusercontent.com/ws4934/many2api/main/scripts/install.sh | sh
#
# Optional environment variables:
#   GROK2API_DIR             install directory (default /opt/grok2api as root, ~/grok2api otherwise)
#   GROK2API_PORT            published port, e.g. 8000 or 127.0.0.1:8000 (default 8000)
#   GROK2API_ADMIN_PASSWORD  admin password for a first install (default: random)
#
# Running it again pulls the latest image and restarts; an existing
# config.yaml and docker-compose.yml are kept.
set -eu

REPO_RAW="https://raw.githubusercontent.com/ws4934/many2api/main"

fail() {
	printf '错误：%s\n' "$*" >&2
	exit 1
}

random_hex() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex "$1"
	else
		head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
	fi
}

random_base64() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -base64 "$1"
	else
		head -c "$1" /dev/urandom | base64 | tr -d '\n'
	fi
}

case "$(uname -m)" in
x86_64 | amd64 | aarch64 | arm64) ;;
*) fail "镜像只提供 linux/amd64 和 linux/arm64，当前机器是 $(uname -m)" ;;
esac
command -v curl >/dev/null 2>&1 || fail "需要 curl"
command -v docker >/dev/null 2>&1 || fail "未安装 Docker，请先安装：https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || fail "未找到 docker compose（需要 Compose v2 插件）"
docker info >/dev/null 2>&1 || fail "无法连接 Docker：请用 root 运行，或把当前用户加入 docker 组"
case "${GROK2API_ADMIN_PASSWORD:-}" in
*[\"\\\|\&]*) fail "管理员密码不能包含 \" \\ | &" ;;
esac

if [ -z "${GROK2API_DIR:-}" ]; then
	if [ "$(id -u)" = 0 ]; then
		GROK2API_DIR=/opt/grok2api
	else
		GROK2API_DIR="$HOME/grok2api"
	fi
fi
mkdir -p "$GROK2API_DIR"
cd "$GROK2API_DIR"
echo "安装目录：$GROK2API_DIR"

if [ ! -f docker-compose.yml ]; then
	curl -fsSL -o docker-compose.yml.tmp "$REPO_RAW/docker-compose.yml"
	mv docker-compose.yml.tmp docker-compose.yml
fi

password=""
if [ ! -f config.yaml ]; then
	password="${GROK2API_ADMIN_PASSWORD:-$(random_hex 12)}"
	curl -fsSL -o config.yaml.tmp "$REPO_RAW/config.example.yaml"
	sed -e "s|replace-with-at-least-32-characters|$(random_hex 32)|" \
		-e "s|replace-with-base64-key|$(random_base64 32)|" \
		-e "s|replace-with-a-strong-password|$password|" config.yaml.tmp >config.yaml
	rm -f config.yaml.tmp
	chmod 600 config.yaml
	grep -q "replace-with" config.yaml && fail "config.yaml 中仍有未替换的占位值"
fi

# Keep the port in .env so later docker compose commands use it too.
if [ -n "${GROK2API_PORT:-}" ]; then
	{
		grep -v '^GROK2API_PORT=' .env 2>/dev/null || true
		echo "GROK2API_PORT=$GROK2API_PORT"
	} >.env.tmp
	mv .env.tmp .env
fi
port="${GROK2API_PORT:-$(sed -n 's/^GROK2API_PORT=//p' .env 2>/dev/null)}"
port="${port:-8000}"

docker compose pull
docker compose up -d

printf '等待服务启动'
healthy=""
for _ in $(seq 1 60); do
	if curl -fsS "http://127.0.0.1:${port##*:}/healthz" >/dev/null 2>&1; then
		healthy=1
		break
	fi
	printf '.'
	sleep 2
done
echo
[ -n "$healthy" ] || fail "服务没有在 2 分钟内就绪，请查看日志：cd $GROK2API_DIR && docker compose logs grok2api"

echo "grok2api 已启动：http://服务器IP:${port##*:}"
if [ -n "$password" ]; then
	echo "管理员账号：admin"
	echo "管理员密码：$password"
fi
echo "配置文件：$GROK2API_DIR/config.yaml（请备份；其中的 credentialEncryptionKey 写入账号后不能更换）"
echo "升级：重新运行本脚本，或在管理后台「关于」页在线更新"
