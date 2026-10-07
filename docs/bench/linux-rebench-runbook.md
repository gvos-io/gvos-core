# Linux 裸机复测手册

本机（Windows + WSL2 + Docker Desktop）的容量数字被虚拟化网络栈拖累，绝对值偏保守。
Linux 裸机复测用于产出 v1 公开报告的数据。此文件是复测手册——在 Linux 机器上按步骤
执行即可复现本仓库的全部基准。

## 前置要求

- Linux 裸机（推荐 8+ vCPU / 16GB+），Docker + docker compose
- 网卡调优（媒体面大并发时）：
  ```bash
  sudo sysctl -w net.core.rmem_max=33554432
  sudo sysctl -w net.core.wmem_max=33554432
  sudo sysctl -w net.core.netdev_max_backlog=5000
  ```
- 本仓库（git clone / rsync）

## 复测步骤

```bash
# 1. 起全栈（PG 模式）
docker compose -f deploy/docker-compose.yml -p gvos up -d --build

# 2. 种子（首次）
docker exec -i gvos-postgres-1 psql -U gvos < migrations/002_seed.sql
docker exec gvos-postgres-1 psql -U gvos -c \
  "UPDATE accounts SET credit=50000000 WHERE username='1000';"
docker restart gvos-gv-core-1   # 重载余额快照

# 3. 全量测试套件（Go 侧，可选）
go test ./... && go test -bench=LookupFast -benchtime=200000x -run=xxx ./internal/rate/

# 4. 容量阶梯（长呼 60s）
for n in 100 500 1000 2000; do
  MSYS_NO_PATHCONV=1 docker run --rm --network gvos_default \
    -v "$(pwd)/test/sipp:/scen:ro" ctaloi/sipp:latest \
    -sf /scen/load_long.xml -inf /scen/dest.csv -m $n -r 30 -l $n -timeout 180s kamailio
done

# 5. CPS 聚合（4 批 x 100 CPS，批间重启 UAS）
for batch in 1 2 3 4; do
  docker restart gvos-demo-gw-1; sleep 4
  for i in 1 2 3 4; do
    docker run -d --network gvos_default --name b${batch}-${i} \
      -v "$(pwd)/test/sipp:/scen:ro" ctaloi/sipp:latest \
      -sf /scen/load.xml -inf /scen/dest.csv -m 100 -r 25 -timeout 60s kamailio
  done
  # 等待 + 收数 + 清理（参照 test/sipp/soak.sh 的轮询写法）
done

# 6. 媒体面（RTP 经 rtpengine）
docker run --rm --network gvos_default \
  -v "$(pwd)/test/sipp:/scen:ro" ctaloi/sipp:latest \
  -sf /scen/load_long.xml -inf /scen/dest.csv -m 200 -r 10 -l 200 -timeout 120s -rtp_echo kamailio

# 7. 24h soak（长稳 + 每波对账）
bash test/sipp/soak.sh 1440 10 2

# 8. 计费一致性终检（CDR vs 账本）
docker exec gvos-postgres-1 psql -U gvos -t \
  -c "SELECT count(*), sum(cost) FROM cdr;"
docker exec gvos-postgres-1 psql -U gvos -t \
  -c "SELECT reason, count(*), sum(delta) FROM balance_ledger GROUP BY reason;"
# 两者金额必须互补为零漂移；统计进报告时记录环境（CPU/内存/内核/网卡）。
```

## 报告模板

复测后把结果追加到 `docs/bench/capacity-report-v0.md` 的对照表，标注：
机器型号、CPU/核数、内核版本、Docker 版本、测试时间。保持与 v0.2 相同的
"诚实披露"格式：数字 + 已知限制。
