# memcache 第四轮性能优化机会评估

- **日期**：2026-10-26
- **对象**：`github.com/ohmountain/memcache`（Go 1.18+，零外部依赖），基线为 `c9d50ea`（第三轮审计后）
- **测量环境**：Linux/amd64，AMD Ryzen 9 7900X（12C/24T），Go 1.27.2
- **方法**：仓库只读基准 + CPU profile（`-cpuprofile`，输出在 /tmp）；验证实验全部在 `/tmp/mcprobe2`（外部探针模块 + 两份库副本），仓库代码未改动
- **前序**：[第三轮优化](2026-10-26-round3-optimizations.md)与其[审计](2026-10-26-round3-audit.md)；本文不重复其结论，只在其边界上继续挖掘

---

## 0. 结论摘要

第三轮之后，单点成本的重心已经转移。当前实测（本机，单线程）：

| 操作 | 耗时 | 剖析结论 |
|---|---:|---|
| Lookup（非 TTL 命中） | 15.1–15.6 ns | map 访问占整条核心路径 ~66% |
| Peek（非 TTL） | 10.9 ns | 已接近纯 map 成本 |
| **Lookup（TTL 命中）** | **~40 ns** | **`runtime.nanotime` 占 38%，比 map 还高** |
| **Peek（TTL）** | **~37 ns** | 同上 |
| Set / RenewTTL | 17 / 75 ns | RenewTTL：堆 sift 42%、时钟 21%、map 18% |
| Get | 30.7 ns, 1 alloc | 对比 Lookup +15 ns（指针逃逸） |
| Len/Stats @64 分片 | ~195 ns | 全部是加解锁往返 |
| 每 entry 常驻 | ~120 B | 原型曾到 99 B |
| Clear+重填 10 万条 | ~11–12 ms | 与全新填充同价，map 增长阶梯全额重付 |

分片模式下每 op 哈希两遍（`hashKey` 7.8% + map 内部 `memHashAES` 8.3% ≈ 16%）。

本轮立项四项（P0 一项、P1 三项、P3 三项），并正式排除若干候选（§3）。
自研哈希表与近似 recency 属 P2 研究项，**本轮不立项**（§4）。

## 1. P0：TTL 读路径改用"写者发布的粗时钟"

TTL 命中读每次调 `time.Since`（本机 ~15 ns vDSO）。语义上只需判断 deadline 是否已过；
而写者在 `set()` 里本来就要在锁内读精确时钟，可以把样本留在分片里。

**设计**：`shard` 增加 `nowSample int64`（分片互斥锁保护，无原子操作、无新增跨核流量）；
发布点为 `set()`（读时钟处）、`purgeDueBatch()`、`Size()`（后两者本就持分片锁且有新样本），
发布用 max 守卫防回退。读者（`lookup`/`peek`）在选项开启时改比 `deadline <= sh.nowSample`。

正确性方向：`nowSample` 是过去的下界，`deadline <= nowSample` ⇒ 必定已过期（删除仍保守正确）；
`deadline > nowSample` 判活，最坏把刚过期条目多服务一个"距上次观测"的窗口。
**这是语义放宽，必须 opt-in**（`Options.ApproximateTTLReads bool`，默认 false 保持精确）。
在持续有写或 cleaner 活跃的分片里陈旧度天然有界；cleaner 的 timer 本就 armed 在最早 deadline。

**实测**（/tmp 库副本，同一基准，单线程）：

| 路径 | 精确时钟 | 粗时钟 | 变化 |
|---|---:|---:|---:|
| TTL 命中 Lookup | 40.2 ns | 16.5 ns | **−59%** |
| TTL Peek | 37.0 ns | 11.4 ns | **−68%** |
| TTL Peek（24 线程/64 分片） | 18.0 ns | 18.6 ns | 持平（锁主导） |

定位为延迟优化：TTL 读掉到非 TTL 水位；高并行分片不亏。

## 2. P1：三项

### 2.1 惰性删除过期堆

`RenewTTL` 75 ns 中堆操作占 42%（`fix` 的 O(log n) 下 sift，真实堆是 `[]*node` 指针数组，
每次比较都是指针追逐）。改为**代际计数 + 记录自带 deadline** 的惰性删除堆：

- 堆记录 `heapRec[T]{n *node[T]; deadline int64; gen uint64}`；`node.expiryIndex` 删除，换成 `heapGen uint64`
  （node 仍 56 B/一条 cache line）。记录自带 deadline 是必须的：若按 node 当前 deadline 排序，
  续约会原地改键、破坏堆序，过期回收会无界滞后。
- 续约 = `heapGen++` + push 新记录（单调续约的新 deadline 近似全局最大，上 sift≈0）；
  取消/删除/覆写 = `heapGen++`（O(1)，不再 `removeAt`）；`release()` 也 bump gen，freelist 复用绝不复活旧记录。
- purge 弹根：`rec.gen != rec.n.heapGen` ⇒ 丢弃并继续；否则完整回收。
- 压缩：`len(heap) > 2*len(m)` 时原地过滤重建（触发点在 push/purge 后），惰性记录内存有界。
- **round-3 唤醒契约不需要重推导**：根上包含陈旧记录只会让 timer 更早醒（安全的过度唤醒方向），
  `rootNeedsWake`/`extends` 快路径/缩短 TTL 重排全部原样保留。
- 副产红利：swap 不再写 node（sift 更便宜），取消路径全部 O(1)。

独立实验（隔离堆机制，随机续约）：fix 13.4/17.9 ns vs lazy 10.5/10.0 ns（n=1K/16K，1.28x/1.79x）；
真实指针堆差距更大，估算 `RenewTTL` 整体 −25~35%。

**前置**：round-3 审计遗留的"确定性多分片 cleaner 测试"测试债必须先补（§2.3）。

### 2.2 小容量分片的槽位上限做成选项

`shardSlotTarget=64` 把 1024 容量缓存钳在最多 16 分片，而同类负载 64 分片 `Peek` 是 16 分片的 2.7x
（99 vs 37 Mops）。最小杠杆：`Options.ShardSlots int`（默认 64），允许小容量开更多分片，
文档写清命中率代价。分片间容量借用（donor 转移）仍留待后续。

### 2.3 确定性多分片 cleaner 测试（测试债，2.1 的前置）

`cleaner_test.go` 的 fixture 全部固定 1 分片；跨分片取最早 root、`purgeDueBatch` 的分批、
多分片下堆清空停表，只被间接覆盖。用现有 `fakeClock` + `newMemcacheWithClockShards`
补 2/4 分片的确定性场景（fake timer hooks 驱动，无 sleep）。

## 3. P3：三项小改

1. **`GetInto(dst *T) bool`**：`Get` 30.7 ns+1 alloc vs `Lookup` 15.5 ns；写入调用方目标即归零。命中填充 dst 返回 true；未命中不动 dst 返回 false。
2. **锁自由 `Len`**：`shard.size` 改 `uint32` + 锁内原子 store（amd64 普通 MOV），`Len` 纯原子读，
   64 分片 ~195 ns → ~20–30 ns。round-3 §5.5 的方案，纯监控口径。
3. **Clear 后重填 hint**：Clear 丢弃 map，重填与全新填充同价（10 万条 ~11–12 ms vs 预分配 7–8 ms）。
   `clearLocked` 记录高水位 `refillHint = min(size, cap, 4096)`，首次建 map 时作 hint。
   不保留 freelist（Clear 的语义就是释放）。

## 4. 明确不立项 / 已排除

| 候选 | 结论 |
|---|---|
| 自研开址哈希表（合并两遍哈希、−20% 内存） | **P2 研究项，不立项**：收益大（map 占 47–66%、两遍哈希 16%、120→~99 B/entry）但风险极高（Go 1.18 兼容、扩容/删除/fuzz 全自担），先出原型数据再决策 |
| 近似 recency（second-chance） | **P2 研究项**：moveFront 占命中 45%、node 有 8 字节余量，但需先跑命中率模拟，opt-in |
| cleaner 原子 root 镜像 | **已排除**：缩短 TTL 插入风暴（24T/64 分片/300 种 TTL）profile 里 cleaner 整体不可见 |
| 并行化 CLEAR_ALL | **已排除**：Clear 已 O(shards)，256 个满缓存 67 µs |
| freelist 上限按分片容量缩放 | **已排除**：实测 free ≤ 活跃数，1024 上限只对大分片生效 |
| 时钟接口去虚化 | **已排除**：profile flat ~0.4% |
| TTL 时钟移出临界区 | 维持 round-3 否决（单分片 24 线程 +27%） |

## 5. 验收口径

- 每项改动：`make check` 全绿（gofmt、golangci-lint v2.15.0、vet、test、shuffled race）；
  Go 1.18 语法兼容（无 1.18 后 API）。
- 性能结论只对"区间不重叠且方向可解释"的对照成立（ABBA 或 benchstat，24 线程方差 10–20%）。
- 行为契约变化只有两处：`ApproximateTTLReads` 选项（新增、默认关）与堆内部结构（不可观察）。
