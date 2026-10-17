#!/bin/bash
# 构建 pi-agent 沙箱镜像（docker 与 Apple container 通吃，BuildKit 同一 Dockerfile）。
#
# 用法：
#   ./build.sh docker            # docker build -t pi-agent:26 .
#   ./build.sh apple             # container build -t pi-agent:26 .  （需 brew install container）
set -euo pipefail
cd "$(dirname "$0")"

backend="${1:-docker}"
tag="pi-agent:26"

case "$backend" in
  docker)
    docker build -t "$tag" .
    ;;
  apple)
    # Apple container 首次使用需先: container system start && container system kernel set --recommended
    container build -t "$tag" .
    ;;
  *)
    echo "用法: $0 [docker|apple]" >&2
    exit 1
    ;;
esac

echo "镜像就绪: $tag（$backend 后端）"
echo "ACP_ARGS 形态: run --rm -i --network … -v <workDir>:$HOME 同径挂载 … $tag pi-acp"
