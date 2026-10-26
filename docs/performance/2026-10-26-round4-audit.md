# 第四轮优化的独立审计

审计对象：`c9d50ea..f942819`（9 个提交，21 个文件，+1385/−164），以及
`2026-10-26-round4-optimizations.md` 里的全部结论。
审计方式：独立评审席对全量 diff 做对抗性走查（不采信实施记录的论证），重跑全部门禁
与代表性基准，用独立测试程序实测结构体偏移。

**结论：未发现 Critical 缺陷。** 惰性删除堆、唤醒契约、原子 size、粗时钟四条并发主线
在全部路径上走查成立；门禁全绿、基准复现、Go 1.18 兼容与向后兼容均独立验证通过。
发现 1 项 Important（计划明确要求的 `Len` 公开文档契约漏更新）与 3 项可当场修掉的
Minor，已全部在本次审计中修复；另有 2 项记录在案不修。

## 1. 已独立核实成立

### 1.1 门禁与基准复现

- gofmt、`go vet`、golangci-lint v2.15.0（仓库 `.golangci.yml`）0 issues、
  `go test -count=1`、`go test -race -shuffle=on -count=3` 全部通过。
- 库代码 `-gcflags=-lang=go1.18` 编译通过；`go.mod` 未动，零外部依赖不变。
- 代表性基准复现：`TTLHit` 40.5 ns vs `TTLHitApprox` 16.5–17.4 ns（cpu=1），与记录的
  −57.6% 相符。
- 实测 `sizeof(shard)=192`、node 56B，`TestIntrusiveLayout` 的 64 字节对齐证明仍在。

### 1.2 惰性删除堆与 heapLive 记账

10+ 个变更点逐一核对：`set` 的新建/续期/取消/缩短（含 expired-target 分支的
`oldDeadline = 0` 复位，无双减、无漏减）、`removeLocked`、`delete` 的过期分支、
`purgeExpiredLocked` 的 stale-discard（`continue` 只弹不删活记录）、
`maybeCompactLocked` 的重同步、`clearLocked`、`rootDeadline` 的 stale-root 弹出——
每条路径上 heapLive == 活记录数。堆序只观察 `heapRec.deadline`（不观察可变的
`node.deadline`）；`popRoot` 不写节点；`compact` 是写索引不超前读索引的原位过滤 +
Floyd 建堆。

代数纪律成立：`heapGen` 的每次自增都在所属分片锁内，节点生命周期内 stale 记录不可
能复活；`release()` 的 bump 是诚实的纵深防御，记录里"不可测"的承认与独立走查一致
（`removeLocked` 的 bump 覆盖所有可达路径）。

### 1.3 唤醒契约

`rootDeadline()` 先弹尽 stale root 再返回，worker 永不 arm 在死 deadline 上；
`delete`/`lookup` 的 `heapLive == 0` 停表信号正确处理"数组里还有 stale 记录"的
情形；`extends` 快路径在 expired-target 分支下仍然成立——弹掉最小活根再压入更晚的
记录只会让活根更晚。

### 1.4 原子 size 与粗时钟

`shard.size` 全部访问点（写者、`Len`、`Size`、stats、`clearLocked`、invariants
测试）均为原子操作，无一处普通访问；写者被分片锁串行化，无丢失更新。`publishNow`
的 max-guard 防止 purge 路程的预等待采样回退较新的写者采样；`deadline <= nowSample`
与下界方向性成立——绝不提前淘汰，lookup 的过期删除分支保守方向正确，边界测试钉住
`<=`。

### 1.5 向后兼容

默认 `ShardSlots` 复现旧 `shardCount`（未被修改的 `TestShardCountRoundsDown` 钉住）；
`ApproximateTTLReads` 默认关闭且关闭时精确（有测试钉住）；`GetInto` 纯增量 API。

## 2. 发现的问题（均已在本次审计中修掉）

| 级别 | 问题 | 处理 |
|---|---|---|
| Important | `Len` 公开 godoc 漏掉计划 Task 6 明确要求的并发契约（"并发 Clear/Close 期间计数可能短暂偏高；静止读精确"）。内部 `cacheState.Len` 的注释有，用户实际读到的公开文档没有，仍暗示旧的持锁一致性 | `memcache.go` 的 `Len` 注释补齐，与内部注释同口径 |
| Minor | shard 布局注释数字过时：写"104 字节 body / 计数器 104–167"，实测 `heapLive`@104、`refillHint`@112、计数器占 120–184（body 到 refillHint 止于 116）。实施记录 §2.4"counters 止于 184"是对的，代码注释与它矛盾——注释精度回归 | 按实测偏移改写 |
| Minor | 计划中 `ShardSlots` 测试表两行算术上不成立（`requested=64` 产不出 128 分片；63/16 下取整是 2 非 1），落地时静默改为正确值（`{1024,128,8,128}`、`{63,64,16,2}`）并补一行 `{15,64,16,1}`，记录未注明该偏差 | 实施记录 §2.3 补一行说明计划表被修正 |
| Minor | `GetInto(key, nil)` 命中时 panic 是目标指针 API 的常规约定但未写明 | 文档补半句 |

记录在案不修的两项：`newCacheState` 参数顺序与计划不同（`slotTarget` 放在 `shards`
后而非队尾——内部测试缝、分组更佳）；计划 Task 5 的 `GetInto` 签名漏 key（明显的
计划笔误，实现签名是唯一合理读法）。

## 3. 不予裁决项

- 无写且 cleaner 睡眠的分片上的 approx 陈旧窗口——记录 §5.1 已文档化的残余风险，
  opt-in 且实际以条目自身 deadline 为界。
- `shard.size uint32` 在每分片 ≥40 亿条时溢出——字段注释已写明设计包络。
- `heapGen uint64` 回绕——不现实，按审计指令忽略。
- 32 位平台布局——`TestIntrusiveLayout` 跳过，文档本就只覆盖 64 位（既有姿态）。
- 巨大 capacity 在 32 位上 `int(capacity)` 溢出——先于本轮存在。
- ABBA 绝对数字漂移——中位数 + 区间重叠方法论已文档化；Shards64-24 的噪声判定
  （重测后同侧 ±40% 方差、完全交错）在 24 线程 10–40% 方差的既知背景下是合理的
  统计判断。

## 4. 残余风险与建议（未修，需决策）

1. **五个唤醒调用点没有单一端到端表覆盖**（记录 §5.2 自列）：建议一张参数化场景表把
   set/缩短/取消/delete/lookup-expired/Clear 对 armed delay 一并钉住；多分片测试覆盖
   大部分，但缺 lookup-expired 与 Clear 的分片化配置。
2. `assertInvariants` 的活记录普查是"压缩重同步无法掩盖记账漂移"的关键守卫——建议
   在该处加注释说明意图，防止后人当实现细节误删。
3. Go 1.18 未实机验证（round-3 审计遗留）：`-lang=go1.18` 只保证语法与 API，
   size class 与对齐行为仍需 CI 实测。

## 5. 复现

```sh
make check
go test -race -shuffle=on -count=3 ./...
GOARCH=386 go test -count=1 ./...      # 32 位回归（round-3 修复的守卫）
# 基准复现（cpu=1，记录口径）
go test -run '^$' -bench 'BenchmarkCache/(TTLHit|TTLHitApprox)$' -benchtime=200ms -cpu=1
```
