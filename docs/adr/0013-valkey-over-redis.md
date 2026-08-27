# ADR-0013：快取 / 冪等 / 限流儲存改用 Valkey（取代 Redis）

- **Status**：Accepted（2026-08）
- **關聯**：ADR-0007（兩層冪等的第一層）；`01-architecture.md` §3 技術選型；`06-security-compliance.md`；`07-deployment.md`；`pkg/idempotency`、`internal/gateway`

## Context

Phase 0 的設計以 Redis 7 作為 api-gateway 的冪等快取、API key 快取、重放 nonce、限流與 provider 健康度視窗儲存（ADR-0007 第一層）。但：

- Redis 自 7.4 起改為 RSALv2 / SSPLv1 雙授權、8.0 起改為 AGPLv3 三選一；對商用閉源部署與供應鏈審查（支付場景的合規盤點）都增加不確定性。
- 主要雲端供應商已轉向 [Valkey](https://valkey.io)（Linux Foundation、BSD-3-Clause、自 Redis 7.2.4 fork）：AWS ElastiCache / MemoryDB 均提供 Valkey 引擎且價格更低；我們的 staging / production 規劃正是 ElastiCache。
- Valkey 與 Redis 為 RESP wire-compatible，我們用到的原語（`SET NX EX`、`GET`、`DEL`、`INCR`+`EXPIRE`、TTL 淘汰語意、`maxmemory-policy`）行為一致，ADR-0007 的設計完全不受影響。

尚未上線（Phase 0 剛收尾、無既有部署與資料），現在切換零遷移成本；上線後再換則要處理線上 failover 與雙寫驗證。

## Decision

全面以 **Valkey 8** 取代 Redis，包含伺服器、client library 與所有命名：

1. **伺服器**：`valkey/valkey:8-alpine`（compose、CI service container、testcontainers）；staging / production 用 ElastiCache Valkey 引擎。
2. **Go client**：`github.com/valkey-io/valkey-go`（官方 client）+ `valkeycompat` adapter。compat 層 API 與 go-redis 同形（`SetNX` / `Get` / `TxPipeline` / `Nil`），介面型別為 `valkeycompat.Cmdable`，遷移為機械式替換。移除 `github.com/redis/go-redis`。
3. **設定**：`PG_REDIS_ADDR` / `PG_REDIS_PASSWORD` → `PG_VALKEY_ADDR` / `PG_VALKEY_PASSWORD`；helm values `global.redis` → `global.valkey`；compose service 名 `redis` → `valkey`。
4. **程式命名**：`RedisStore` → `ValkeyStore`、`RedisRateLimiter` → `ValkeyRateLimiter`、`RedisReplayDetector` → `ValkeyReplayDetector`；readyz check key `redis` → `valkey`。
5. **文件**：docs/01–11、README、HANDOFF、openapi、proto 註解全面改為 Valkey；歷史 ADR（0007）不改寫，加註讀作 Valkey。

## Consequences

### 正面

- 授權疑慮消除（BSD-3-Clause），供應鏈審查單純。
- 與 ElastiCache Valkey 引擎對齊，成本較 Redis 引擎低約 20–30%。
- wire-compatible：ADR-0007 的冪等語意、fail-closed 行為、LRU 淘汰策略全部不變；風險僅在 client library 更換。
- 趁 Phase 0 尚未部署完成切換，無資料遷移、無雙寫期。

### 負面 / 代價

- `valkey-go` 較 go-redis 年輕；以 `valkeycompat` adapter 隔離（API 同形），若有問題可低成本換回任一 RESP client。
- `valkey-go` 預設走 RESP3 與 auto-pipelining，行為與 go-redis 有差異（對我們用到的原語無影響，但整合測試必須跑真 Valkey——已涵蓋於 `pkg/idempotency` integration test）。
- 環境變數改名是 breaking change；因尚無任何部署，僅需同步文件與 deploy 設定（本 ADR 已一併完成）。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 只換伺服器、留 go-redis client | 可行（go-redis 支援 Valkey），但 repo 內殘留 `redis/*` 相依與命名，授權盤點仍要解釋；Phase 0 全換的成本極低。 |
| 留在 Redis CE 8（AGPLv3） | AGPL 對商用閉源部署的法務審查成本高；雲端 managed 版本另有商業授權費。 |
| KeyDB / Dragonfly | KeyDB 社群動能弱；Dragonfly 為 BSL 授權，同樣有授權疑慮。 |
| 上線後再換 | 需要線上 failover、雙寫驗證與回滾計畫；現在換是零成本視窗。 |

## 對 01 文件的影響

01 §3 技術選型表「快取 / 冪等 / 限流」由 Redis 7 改為 Valkey 8（已更新）；其餘章節僅名詞替換，架構不變。
