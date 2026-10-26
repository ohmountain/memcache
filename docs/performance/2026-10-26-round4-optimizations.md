# memcache 第四轮性能优化：粗时钟 TTL 读、惰性删除过期堆与三个小项

- **日期**：2026-10-26
- **对象**：`github.com/ohmountain/memcache`（Go 1.18+，零外部依赖），对照基线 `c9d50ea`（第三轮审计后）
- **测量环境**：Linux/amd64，AMD Ryzen 9 7900X（12C/24T），Go 1.27.2
- **对照方式**：`git archive c9d50ea` 导出 control 树并补齐新增基准（两边测同一集合），ABBA 交错
  （control → work → work → control，`-benchtime=200ms`），按基准取中位数、检查区间重叠；疑似项单独复测
- **计划**：[2026-10-26-round4-perf-optimizations.md](../superpowers/plans/2026-10-26-round4-perf-optimizations.md)；
  机会评估与设计依据见 [2026-10-26-round4-opportunities.md](2026-10-26-round4-opportunities.md)

---

## 1. 结论摘要

1. **TTL 续约路径 −38~40%**：`RenewTTL` 77.2 → 46.4 ns（`-cpu=1`）、76.2 → 47.2 ns（`-cpu=24`），
   `ShardedSetExpire/Shards1` −37.1%、`Shards8` −33.5%、`Shards64` −23.3%。来源：过期堆改为
   **代际计数的惰性删除**——续约 push 新记录（单调续约零 sift）、取消/删除只 bump 代数，swap 不再写 node。
2. **TTL 命中读 −57.6%**（`Options.ApproximateTTLReads` 开启时）：`TTLHitApprox` 37.9 → 16.1 ns，
   与非 TTL 读（15.9 ns）持平。语义放宽（过期条目可多服务一个观测窗口）是 opt-in，默认精确。
3. **锁自由 `Len`**：`shard.size` 改 uint32 原子读写，Len 不再逐分片加锁（64 分片从 ~195 ns 锁往返降为若干原子读）。
4. **`GetInto(key, dst *T) bool`**：零分配读（`Get` 每次分配 8B 指针并逃逸）。
5. **Clear 重填 hint**：Clear 记录 `min(size, cap, 4096)`，首次建 map 预设容量，重填跳过大部分增长阶梯。
6. 行为契约变化只有 `ApproximateTTLReads`（新增、默认关）；堆内部结构不可观察。`make check` 全绿
   （含 `-race -shuffle=on -count=3`），期间抓到并修复了两个自己引入的缺陷（见 §4）。

## 2. 已实施改动与实测

### 2.1 GO-PERF-R4-01：`Options.ApproximateTTLReads`（粗时钟 TTL 读）

- `shard.nowSample int64`（分片锁保护，无原子操作）；发布点：`set` 读时钟处、`purgeDueBatch`、`Size`，
  带 max 守卫防回退；读者（`lookup`/`peek`）在选项开启时比 `deadline <= nowSample`。
- `deadline <= nowSample` ⇒ 必定已过期（样本是下界的方向性保证删除仍保守正确）；
  `deadline > nowSample` 判活是唯一的语义放宽。
- `shard` 布局：+8B（nowSample），pad 32→24，仍 192B/三条 cache line（`TestIntrusiveLayout` 钉住）。
- 测试：`approximate_read_test.go` 六项（默认精确、未观测窗口命中、发布后 miss+删除、边界相等、
  cleaner 发布、publishNow 单调性/门控）。

### 2.2 GO-PERF-R4-02：代际惰性删除过期堆

- 记录 `heapRec{n, deadline, gen}` **自带 deadline 快照**（按 node 当前 deadline 排序会在续约时
  原地改键、破坏堆序，过期回收无界滞后——这是实现前推导出的关键设计约束）；
  `node.expiryIndex` → `heapGen uint64`（等宽，node 保持 56B/一条 cache line）。
- 续约 = bump + push；取消/删除/覆写 = bump（O(1)，不再 `removeAt`）；`release` 也 bump
  （对 `removeLocked` 的冗余防御，变异测试确认无行为路径依赖它）。
- purge 弹根时按 `rec.gen != rec.n.heapGen` 丢弃陈旧记录；压缩触发
  `len(heap) >= 64 && len(heap) > 2*len(m)`，设在每个堆增长或 map 缩小的操作尾部
  （不在 `removeLocked` 内，保持 purge 循环摊销）。
- **维持 round-3 唤醒契约的两件配套**（实现中由既有测试逼出）：
  - `rootDeadline()` 弹掉堆顶陈旧记录——否则 timer 会 arm 在没有活跃条目的 deadline 上，空堆停表被无限推迟；
  - `shard.heapLive` 活跃计数——lookup/delete 的停表唤醒条件从 `len(heap)==0` 改为 `heapLive==0`，
    陈旧记录滞留时也能停表。
- `assertInvariants` 重写为：按 `rec.deadline` 有序；每个有 deadline 的 map 条目恰一条活跃记录、
  无 deadline 的条目零条、freelist 节点零条；操作后 `len(heap) < 64 || len(heap) <= 2*len(m)`；
  `heapLive` 与活跃记录普查一致。

### 2.3 GO-PERF-R4-03：`Options.ShardSlots`

`shardCount` 的槽位下限参数化（≤0 取默认 64）。1024 容量配 `ShardSlots: 16` 可用 64 分片
（原上限 16 分片）。表驱动钉住纯函数（含 0/负值/幂对齐/容量小于下限），行为测试钉
Σcap == capacity、低密度填充不丢、超容量填充 ≤ 总容量。

### 2.4 三个小项

- `GetInto`：命中写 `*dst` 返回 true，未命中不动 `*dst` 返回 false；`AllocsPerRun == 0`。
- 锁自由 `Len`：**所有** size 访问原子化（写仅 `atomic.StoreUint32`，读一律 `atomic.LoadUint32`——
  锁内的普通写与 Len 的原子读并发即数据竞争，这是 race 套件抓出的，见 §4）。
  `TestLenCompletesWhileShardLocked` 用"持锁期间 Len 必须返回"钉住无锁性质。
- Clear hint：`refillHintFor(size, cap)`（0/实际值/min(cap, 4096)），`shard.refillHint int32` +
  对齐空隙使 shard 仍 192B（pad 32→24→16→8 的最终账：counters 止于 184，pad 8）。

## 3. 测试与变异自检

- 新增测试文件：`approximate_read_test.go`、`cleaner_multishard_test.go`（Task 3 前置）、
  `lazy_heap_test.go`、`shard_slots_test.go`、`getinto_test.go`、`len_lockfree_test.go`、`clear_hint_test.go`。
- 变异自检（改坏 → 目标测试必须红 → 还原）：
  - `purgeDueBatch` 只扫第一个分片 → 多分片测试红；`rootNeedsWake` 恒假 → 缩短 root 测试红（Task 3）。
  - 删除 purge 的陈旧丢弃 → `TestSetAfterExpireSurvivesStaleRoot` 红。
    **该测试第一版被掩盖**（被取消的记录恰在根上，被 `set()` 自己的 rootDeadline-after 弹掉），
    强化场景为"陈旧记录前面挡一条更早的活跃根"后才真正钉住。
  - 删除压缩触发 → `TestHeapCompactionBound`/`TestRenewalLeavesOneLiveRecord` 红。
  - 删除 `release` 的 gen bump → 无测试可红：`removeLocked` 的 bump 覆盖一切可达路径，
    该 bump 是纯防御，保留并如实记录。
- 适配两个钉旧契约的测试：`TestExpiryMetadataIsBounded`、`TestForegroundExpiryLiveVictimAndRenewal`
  从"恰好 N 条记录"改为"恰好 N 条**活跃**记录"（新增 `liveExpiryLen` 助手）。

## 4. 过程中抓到的缺陷（均已被测试钉住）

1. **惰性堆第一版漏了 delete/lookup 过期路径的压缩触发**：模型差分测试（`TestModelLargeSequences`）
   抓到 `heap=124 > 2×61` 破界。
2. **heapLive 记账漏了续约分支的旧记录递减**：缩短 TTL 的测试抓到 `heapLive=1 但 0 活跃`。
3. **size 原子化第一版在锁内留了普通写**：`-race` 抓到与 Len 的原子读竞争；修正为全原子访问。
4. **`TestApproximateReadsCleanerPublishes` 初版手工 fire fake timer**：与 worker `arm()` 的
   Stop→Reset 窗口竞争丢事件（shuffle race 套件 ~1/10 概率挂），改走 `notifyCleaner` 生产唤醒通路。

## 5. 残余风险

1. `ApproximateTTLReads` 的陈旧窗口在"纯读、无写、cleaner 休眠"的分片上无界（直到该分片下一个
   deadline 到点 cleaner 醒来发布）。文档已写明；真实负载里写活跃分片一个操作内收敛。
2. 惰性堆的唤醒正确性依赖"`rootDeadline` 弹陈旧根 + `heapLive` 停表"两个不变量，
   仍无五个调用点的端到端自动覆盖（round-3 遗留，本轮多分片测试补了主要路径）。
3. Go 1.18 未实机验证（本机 1.27.2；语法门禁靠 `-lang=go1.18` 与 lint）。
4. 24 线程方差 10-40%：`Sharded/Shards64-24` 在 ABBA 表中标记 +7.7%（CLEAR），单独复测三组
   C/W 完全交错、同侧波动即 ±40%，判为噪声，无可分辨回归。
5. 原始测量输出未入仓（`/tmp` 即弃）；本文数字可按 §6 重跑复核。

## 6. 复现

```sh
# 对照（基线 c9d50ea + 补齐新基准）
rm -rf /tmp/r4control && mkdir -p /tmp/r4control
git archive c9d50ea | tar -x -C /tmp/r4control
cp benchmark_test.go /tmp/r4control/
# control 树中把 TTLHitApprox 的构造替换为 WithLRU[int](1024, true)（无该选项）
# 然后 ABBA 交错各跑 -bench 'BenchmarkCache$|Sharded' -cpu=1,24

make check
```

## 7. 各任务提交

| 提交 | 内容 |
|---|---|
| `f6dbad4` | feat: Options.ApproximateTTLReads（含 TTLHit/TTLHitApprox 基准） |
| `ca1d2fa` | feat: Options.ShardSlots |
| `2e267fe` | test: 确定性多分片 cleaner 覆盖（Task 4 前置） |
| `d7c9ce9` | perf: 代际惰性删除过期堆 |
| `b2081c6` | feat: GetInto |
| `8a5ac72` | perf: 锁自由 Len |
| `0faf89f` | perf: Clear 重填 hint |
| `60f7d71` | test: cleaner 测试改走 wake 通路（flaky 修复） |
