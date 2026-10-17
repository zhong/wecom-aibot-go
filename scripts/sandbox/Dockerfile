# pi-agent 沙箱镜像：node + pi + pi-acp，运行用户非 root。
# 同一镜像被 docker（OrbStack）与 Apple container 双跑（OCI 通用）。
#
# 版本备注（2026-10 实测）：
#   - node 必须 26：18 下 pi-acp 崩 "crypto is not defined"。
#   - pi-acp 需要可写的用户目录（~/.pi/pi-acp），HOME 相关落位见
#     docs/sandbox-integration-sketch.md。
FROM node:26-alpine

RUN addgroup -S pi && adduser -D -S -G pi pi \
 && npm install -g @earendil-works/pi-coding-agent@1.3.0 pi-acp@0.0.34

USER pi
WORKDIR /home/pi
