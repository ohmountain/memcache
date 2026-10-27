# memcache 性能优化机会评估：从单点到架构

- **日期**：2026-10-26
- **对象**：`github.com/ohmountain/memcache`（Go 1.18+，零外部依赖）
- **测量环境**：Linux/amd64，AMD Ryzen 9 7900X（12 物理核 / 24 线程），Go 1.27.2
- **既有基线**：`docs/performance/2026-10-25-baseline.md`（Apple M1 Pro，8 线程，Go 1.27.0）
- **运行时改动**：**无**。本文所有“原型”代码位于本地 `.perf-lab/`（以点开头的隐藏目录，Go 工具链不会扫描），只用于验证优化量级，不是生产实现。
- **“架构”范围**：本项目是进程内缓存库，这里的架构指**并发模型、存储布局、TTL 调度、生命周期/注册表、淘汰策略与对外 API 契约**，不涉及部署拓扑。
- **实施状态**：P0/P1 与低风险 P2 已落地，结果见 [实施记录](2026-10-26-optimization-implementation.md)；淘汰策略、时间轮、字节容量等仍按本文建议留待模拟/设计。**本文的性能数字来自 `.perf-lab/` 原型或预测，不是最终实现的验收数据**；最终实测、门禁对照与未达项（footprint、空闲内存、分片高核数吞吐）以实施记录 §2.1 为准。

---

## 0. 结论摘要

先给最重要的五个判断：

1. **最大的单点收益不是“更快的 LRU”，而是“不必要的工作”。**
   非 TTL 缓存的一次 `Set` 目前固定读一次时钟（本机 `time.Now()` ≈ 30.7ns），占 54.4ns 的 **56%**；把时钟读取改成惰性后，`Set` 降到 **23.6ns（−57%）**。TTL 已启用、但当前没有任何 deadline 的普通 `Set` 同样从 **67.2ns → 33.3ns（−50%）**。这是三行级改动，语义不变。

2. **第二个确定性收益是内存布局和分配。**
   用“侵入式双向链表 + 节点复用 + int64 单调 deadline + 专用堆”替代 `container/list` + `time.Time` + `container/heap`，在单线程 1K 容量下：`Set` **23.6 → 18.5ns**，`InsertEvict` **88 → 33ns**，分配 **55 → 0 B/op**，每 entry 常驻内存 **147 → 99B（−33%，含按分片预分配的 map 空间）**，`RenewTTL` **147 → 72ns**。

3. **并行扩展性已经是负的。**
   24 线程、共享 key 集下，当前全局锁的 `Lookup` 只有 **12.1 Mops/s**，低于单线程的 **55.8 Mops/s**：核越多越慢。16/64 分片原型能到 **29.7 / 71.4 Mops/s**，相对 base 是 2.5x / 5.9x，但相对单线程只有 0.53x / 1.28x。**分片解决锁竞争，不解决缓存行一致性。**

4. **读路径的本质问题是：精确 LRU 的每次命中都是一次写（`MoveToFront`）。**
   24 线程共享 key 集下，即使 64 分片也只能到 71 Mops/s；不刷新 recency 的 `Peek` 在 64 分片下到 **213 Mops/s**，无锁只读 map 到 **1388 Mops/s**。若目标是多核线性扩展，必须让读路径不写共享状态（`Peek` / RCU 快照 / 近似 LRU），而不只是加锁分片。

5. **尾部延迟的最大风险是 `Size()` 的全局清理。**
   100K 过期条目下 `Size()` 持锁 **7.9ms**，期间所有读写被阻塞（仓库 1M 场景为 **158.5ms（Stage A）/ 175ms（B2）**）；16 分片把最大阻塞降到 **0.68ms**，但总清理时间反而略升（11.8ms）。监控 API 不应在全局锁内做 O(k log n) 的全量清理。

### 优先级总表

| 优先级 | 优化点 | 实测/预期收益 | 风险 | 工作量 |
|---|---|---|---|---|
| **P0** | 非 TTL / 无 deadline 写路径去掉 `time.Now()` | `Set` 54.4→23.6ns；TTL 缓存普通 Set 67.2→33.3ns | 极低（纯惰性求值） | 0.5 天 |
| **P0** | 侵入式链表 + 节点复用 + int64 deadline + 专用堆 | `InsertEvict` 88→33ns、0 B/op、内存 −33%、`RenewTTL` 147→72ns | 中（内部数据结构重写） | 2–3 天 |
| **P1** | `Size()` 清理批量化/异步化 / 新增 `Len()` | 消除 7.9ms（100K）～175ms（1M）的全锁停顿 | 高（API 语义） | 3–5 天 |
| **P1** | 分片（opt-in） | 24 线程 12.1→71.4 Mops/s；8 线程约 3–5x；Zipf 命中率不掉 | 中高（近似 LRU、TTL 调度） | 8–10 天 |
| **P1** | `Peek` 无更新读路径 | 24 线程 23→213 Mops/s（64 分片）；接近线性需无锁方案 | 中（语义：不刷新 LRU） | 2–3 天 |
| **P1** | cleaner 懒启动 | 1000 个未用 TTL 的实例省 1000 goroutine、1.31MB | 低 | 1 天 |
| **P1** | 清理批大小自适应/时间盒 | 当前 64/batch，回收 1.52x 基线；减少锁往返 | 低 | 1–2 天 |
| **P2** | 仅“更早 root”唤醒 | 实测仅 3–4%，**不建议**为它改契约 | 低收益 | 0 |
| **P2** | 时间轮替代堆 | `RenewTTL` 有望再降；O(1) 更新 | 高（精度/取消/内存） | 5–8 天 |
| **P2** | 淘汰策略（2Q/SLRU/S3-FIFO/TinyLFU） | 提升 scan 混合负载命中率；需先模拟 | 高（语义/CPU） | 5–10 天 |
| **P2** | 字节/权重容量 | 真正约束内存；避免 1M×4KB=4GB | 中 | 3–5 天 |
| **P2** | Stats/命中率指标 | 可观测、可调容量；热路径 ~1–2ns | 低 | 1–2 天 |
| **P2** | 批量 API（GetMulti/SetMulti） | 摊薄锁与堆操作；多 key 场景 | 低 | 2–3 天 |
| **P2** | 注册表/finalizer/CLEAR_ALL | 高 churn 场景降低全局锁与 GC | 低 | 1–2 天 |
| **P2** | 分片 TTL 调度器/共享调度器 | 多实例省 goroutine；跨分片公平性难 | 高 | 5–10 天 |

---

## 1. 方法与证据边界

### 1.1 为什么要在 24 线程上重测

仓库的权威基线是 Apple M1 Pro、8 线程。8 线程下全局锁的争用已经可见（`ParallelLookup` 从 23.5ns 退到 294.8ns），但**24 线程会把“锁竞争”升级为“缓存行一致性 + 调度”问题**，结论不同：分片相对 base 在 8 线程约 3.1–5.5x、24 线程约 4.7–5.9x；但相对单线程吞吐只有 1.3–1.7x（24 线程）。本文不替代仓库基线，只补充“高核数”这一维度的证据。

### 1.2 原型变体

`.perf-lab/` 中每个变体都是运行时文件的独立副本，仅包名不同：

| 变体 | 与当前实现的差异 | 用途 |
|---|---|---|
| `base` | 直接 import 仓库当前代码 | 基线 |
| `notime` | 仅在 `enableExpired == false` 时跳过 `now()` | 隔离“非 TTL 时钟”成本 |
| `lazy` | `now()` 只在“目标有过期时间 / 本次要写 TTL / 满且堆非空”时采样 | 隔离“TTL 缓存但无 deadline”成本 |
| `earlier` | 在 `lazy` 基础上，cleaner 只在 root 变早/变空/从空变有时唤醒 | 隔离“每次 root 变化都唤醒”成本 |
| `fast-sN` | 新写原型：N 个分片、每分片独立互斥锁/map/侵入式 LRU/索引堆/freelist；int64 单调 deadline；无 cleaner | 验证分片 + 数据结构的总量级 |
| `readonly` + `Peek` | `base` 换成 `sync.RWMutex`，新增不刷新 LRU 的 `Peek` | 隔离“命中写 LRU”的成本 |

### 1.3 测量口径与局限

- 所有数字为本机 **中位数**（2–3 次重复）；高核数下 run-to-run 方差明显，报告只对“方向性/量级”下结论，不把 5% 以内的差异当胜负。
- `base/notime/earlier/lazy/readonly` 通过 `cache` 接口调用，多一次接口分派（约 1–2ns）；`fast` 同样走接口。**变体之间可比，绝对值比仓库直接调用高几 ns。**
- `fast` 原型**没有实现 cleaner**，因此它的 `RenewTTL` 不含唤醒成本；`earlier` 用来单独量化唤醒成本。
- 机器是 12 物理核 / 24 SMT 线程。`GOMAXPROCS=24` 时并非 24 个完整物理核，SMT 与频率会压低线性度；分片结论按“>8 核收益递减”表述，不承诺 24x。
- 没有做跨机器/跨 OS 复现；仓库的 A/B 门禁仍应以 `make perf-compare` 为准。

---

## 2. 现状画像：钱花在哪

### 2.1 单线程核心操作（1K 容量，cpu=1）

| 操作 | base | notime | earlier | fast-s1 | fast-s16 | fast-s64 | 备注 |
|---|---:|---:|---:|---:|---:|---:|---|
| `Lookup` 命中 | 18.7 | 18.2 | 18.8 | **15.7** | 17.7 | 18.6 | 仓库直接调用为 16.7；接口分派约 +1–2ns |
| `Set`（无 TTL） | 54.4 | **23.6** | 26.1 | **18.5** | 31.1 | 30.0 | 仅去掉时钟即 −57% |
| `InsertEvict`（2x key 池） | 119.2 | 88.3 | 95.6 | **33.2** | 34.8 | 34.8 | B/op：55 → **0** |
| `RenewTTL`（续 60s） | 152.3 | 150.3 | 146.8 | **72.1** | 78.1 | 66.8 | 唤醒只占 3–4%；堆/时间表示才是大头 |
| TTL 命中 `Lookup` | 50.5 | 50.6 | 50.7 | **34.2** | 36.3 | 37.3 | 时钟读取代了约 32ns |
| TTL 缓存上的普通 `Set`（当前无 deadline） | 67.2 | 66.9 | — | 37.1 | — | — | `lazy` 为 **33.3**；`fast` 仍每次采样时钟 |

> `base` 的 54.4ns 与仓库基线 61.6ns 同量级（本机更快）；`fast` 的 `InsertEvict` 33ns 与仓库 112ns 的差距主要来自“零分配 + 侵入式链表 + 无时钟”。

**解读：**

- `Set` 的成本结构是“一次时钟 + 一次 map 查找 + 一次链表移动 + 一次可能的堆操作”。对无 TTL 缓存，时钟和堆都不该存在；对 TTL 缓存但当前无 deadline 的普通 Set，时钟也不该存在。
- `InsertEvict` 的 55 B/op 是每个新节点都分配 `node` + `list.Element`（以及可能的 map bucket）。在 scan/写多读少场景，这是 GC 压力的主要来源。
- `RenewTTL` 的 152ns 中，`earlier` 只省 5.5ns，说明**“每次 root 变化都唤醒 cleaner”不是主要成本**；主要成本是 `time.Time` 比较 + `container/heap` 的接口分派 + O(log n) 交换。这与 B2 报告“RenewTTL 因唤醒回退 50%”的推断不同——那份数据来自“每次续约都改变当前最早 deadline”的特定循环，不能外推。**优化前应在目标 workload 上复测，而不是直接改契约。**

### 2.2 并行扩展性：负斜率

自定义并行 harness（每 worker 私有迭代计数，避免依赖 `testing.PB.Next` 的版本差异：Go 1.18 是每次迭代一个共享原子，新版本已改为批量领取），共享 1024 个 key、`GOMAXPROCS` 从 1 到 24，单位 Mops/s：

| 实现 | 1 | 2 | 4 | 8 | 12 | 16 | 24 |
|---|---:|---:|---:|---:|---:|---:|---:|
| base `Lookup` | 55.8 | 41.0 | 32.1 | 28.1 | 22.7 | 13.9 | **12.1** |
| fast-s16 `Lookup` | 57.1 | 47.6 | 50.9 | 49.7 | 46.6 | 28.6 | **29.7** |
| fast-s64 `Lookup` | 55.6 | 62.6 | 88.7 | 88.2 | 62.8 | 67.0 | **71.4** |
| base `Set` | 18.3 | 17.4 | 14.8 | 12.6 | 12.5 | 13.2 | **11.8** |
| fast-s64 `Set` | 32.5 | 31.5 | 54.8 | 69.2 | 49.4 | 50.3 | **55.9** |

补充对照（同一台机器、24 线程）：

| 场景 | Mops/s | 说明 |
|---|---:|---|
| 无锁只读 map（同一 1024 key） | **1388** | 内存/哈希本身可以线性扩展 |
| 每 worker 私有 mutex | **5060** | 私有缓存行上的 lock/unlock 可以线性扩展 |
| 按 hash 选 64 个共享 mutex（只 lock/unlock） | **~250–360** | 共享缓存行的所有权转移成为上限 |
| base + RWMutex 的 `Peek`（单锁、不写） | **23** | 读锁仍写同一缓存行 |
| fast-s64 `Peek`（不写 recency） | **213** | 分片 + 不写入口，仍受锁/缓存行限制 |
| fast-s64 `Lookup`（写 recency） | **71** | 命中写 LRU 把上限再砍到 1/3 |

**解读：**

- 单个全局锁在 24 线程下让 `Lookup` 比单线程**慢 4.6x**；这是“负扩展”，不是“扩展不足”。
- 分片把绝对吞吐拉回并超过单线程，但**远非线性**。原因不是 map 或哈希（无锁 map 可到 1.4B ops/s），而是：
  1. 每次操作仍要 lock/unlock 一个被多个核轮流使用的 mutex；本机按 hash 选 64 个共享 mutex 的**纯 lock/unlock** 只有约 250–360 Mops/s（私有 mutex 是 5B ops/s），这是第一个上限；
  2. 命中还要写 entry/list；去掉这个写（`Peek`）能从 71 到 213 Mops/s，但仍受 mutex 限制；
  3. **disjoint key 实验没有改善 s64**（24 线程 70.7 vs 71.4 Mops/s）：给每个 worker 私有 key 区间并不能恢复线性，说明“同一 hot key 的 entry 缓存行”不是唯一原因，共享 mutex 与分配器布局同样重要。
- 因此“加 16 个分片就线性扩展”是不成立的。**分片是必要的，但不是充分的；读多写少场景要配合无更新读路径，追求线性则要无锁读。**

### 2.3 内存与分配

| 指标 | base | fast-s1 | 变化 |
|---|---:|---:|---:|
| 每常驻 entry 的 live heap（100K 容量，满） | 147.3 B | **99.0 B** | **−32.8%** |
| 1M entries 估算 | ~147 MB | ~99 MB | **省 ~48 MB** |
| `InsertEvict` 分配 | 55 B/op | **0 B/op** | 消除 node + list.Element + bucket 抖动 |
| 1024 个 key 全驻留命中率 | 100% | 100%（s1） | 精确 LRU |
| 16 分片全驻留命中率 | — | **98.2%** | 分片容量切分的代价 |
| 64 分片全驻留命中率 | — | **94.3%** | 分片过多不划算 |
| Zipf(1.2) 命中率（10 万 key，1024 容量） | **79.65%** | 79.58%（s64） | 倾斜负载下几乎无损 |

内存账可以拆开看：base 每 entry 是 `node`（key 16 + value 8 + element 8 + `time.Time` 24 + index 8 ≈ 64B）+ `list.Element`（≈40B）+ map bucket 摊销；fast 是 `entry`（key 16 + value 8 + prev/next 16 + deadline 8 + index 8 = 56B）+ map。**`container/list` 的 `Element` 和 `time.Time` 是两个纯开销。**

### 2.4 TTL 子系统

| 观察 | 数据 |
|---|---|
| 非 TTL 写不必要读时钟 | `time.Now()` 30.7ns/次；`Set` 54.4→23.6ns |
| TTL 缓存无 deadline 时仍读时钟 | 67.2→33.3ns（lazy） |
| TTL 命中读时钟 | 50.5ns vs 非 TTL 18.7ns |
| 续约成本 | 152ns；其中唤醒仅 3–4%（earlier 146.8ns） |
| 堆操作 | `container/heap` 接口分派 + `time.Time` 比较；换专用 int64 堆后 72ns |
| `Size()` 全量清理 | 100K：7.9ms；1M：Stage A 158.5ms / B2 175ms |
| 清理批大小 | 64/batch；100K 需要 1562 次 lock/unlock + `Gosched` |
| 每实例 cleaner | 1000 个 TTL 实例 → 1000 goroutine、+1.31MB（1.35KB/实例） |
| 空堆无定时器 | 已由 B2 解决，保留 |

**一个容易误判的点：** B2 报告把 `RenewTTL` 在高 CPU 下的回退归因于“唤醒”，但本轮在 `earlier` 变体上只测到 **3.6%** 的改善。真正的大头是 `time.Time` + `container/heap`。所以：
- **不要**为了 `RenewTTL` 去改“唤醒契约”；
- **应该**换 int64 deadline + 专用堆（或时间轮）。

### 2.5 生命周期与监控

- `Size()` 是唯一会在读锁内做 O(k log n) 全量清理的公开 API；`CLEAR_ALL()` 串行清理快照，最坏是“所有大缓存各停一次”。
- `lives` 是所有实例共享的一个 `sync.RWMutex` + `map`；创建/关闭在高 churn 下会串行。
- 每个 `Memcache` 都 `runtime.SetFinalizer`；显式 `Close` 会清掉，但短生命周期实例会给 finalizer 队列增加压力。
- `Clear()` 通过 `make(map)` 换新 map，旧 map 等 GC；1M 容量时会出现“新旧两份 map 同时存活”的瞬时峰值。
- `Cap()` 无锁（不可变），`Size()` 有锁且清理，`Lives()` 全局读锁。

### 2.6 已有成果：不要回退

Stage B 已经解决了 TTL 前台的几个核心问题，后续优化必须保留：

- 普通写入不再全量清理过期项，满容量只回收一条（B1）；
- 按最早 deadline 调度、批量 64、空堆不建 timer（B2）；
- Lookup 命中/未命中的热路径不做 scheduler 通知（B2 复审修复）；
- 空闲 TTL 实例 3 秒内 `tail_now_checks = 0`，没有周期性唤醒。

本文的“单点优化”是在这些成果之上做增量，不是重新设计 TTL 语义。

### 2.7 热点位置索引

行号基于本轮 HEAD `cbba0f7`；改动后请重新定位。

| 热点 | 位置 | 现状 |
|---|---|---|
| 写路径无条件读时钟 | [cache_state.go:54](../../cache_state.go#L54) | `now := s.now()`，非 TTL 也执行 |
| 新 key 的 node 分配 | [cache_state.go:76](../../cache_state.go#L76)、[node.go:11](../../node.go#L11) | 每 entry 一个 node + 一个 list.Element |
| TTL 命中读时钟 | [cache_state.go:112](../../cache_state.go#L112) | 每个有 deadline 的命中都 `s.now()` |
| `Size()` 全量清理 | [cache_state.go:205](../../cache_state.go#L205) | 锁内 `purgeExpiredLocked` |
| cleaner 无条件启动 | [memcache.go:52](../../memcache.go#L52) | TTL 能力开启即 `go s.runCleaner()` |
| 固定批大小 | [cleaner.go:9](../../cleaner.go#L9) | 64/batch |
| root 变化即唤醒 | [cache_state.go:170](../../cache_state.go#L170) | 非阻塞 one-slot send |
| 全局注册表 | [monitor.go:12](../../monitor.go#L12) | 单 `RWMutex` + map |
| 每实例 finalizer | [memcache.go:59](../../memcache.go#L59) | `runtime.SetFinalizer` |

---

## 3. 单点优化（按收益/风险排序）

### 3.1 P0：把时钟读取改成惰性

**问题**：`cacheState.set` 无条件 `now := s.now()`，但 `now` 只用于三件事：
1. 目标 key 已有 deadline 时判断是否过期；
2. 本次要写 TTL 时计算 deadline；
3. 满容量时判断堆根是否已过期（决定淘汰堆根还是 LRU 尾）。

对非 TTL 缓存，三件事都不可能发生；对 TTL 缓存，如果目标没有 deadline、本次也不写 TTL、堆为空，同样不需要时钟。

**实测**：
- 非 TTL：54.4 → **23.6ns（−57%）**。
- TTL 缓存、当前无 deadline：67.2 → **33.3ns（−50%）**。
- `time.Now()` 本身 30.7ns，解释了绝大部分差异。

**实现**（`lazy` 变体已验证；示意，省略了 `expiring` 归一化与“过期目标移除”块）：

```go
n, ok := s.holder[key]
needNow := expiring
if ok && !n.expiresAt.IsZero() {
    needNow = true
}
if !ok && uint(len(s.holder)) >= s.capacity && len(s.expiry) > 0 {
    needNow = true
}
var now time.Time
if needNow {
    now = s.now()
}
```

关键正确性点：
- 目标有 deadline 时 `needNow` 必为 true，`!n.expiresAt.After(now)` 不会拿零值 `now` 误判；
- 满容量且堆非空时 `needNow` 必为 true，`reclaimSlotLocked(now)` 的“堆根是否过期”判断有效；
- 堆为空时 `reclaimSlotLocked` 直接取 LRU 尾，不需要 `now`。

**风险**：低。需要补 fake-clock 测试覆盖“非 TTL / TTL 无 deadline / TTL 有 deadline / 满容量 + 堆空 / 满容量 + 堆非空”五条路径。

**验收**：`BenchmarkCache/Set` 在非 TTL 下 ≥30% 改善；`SetExpire` 路径无回退；`make check` + shuffled race。

---

### 3.2 P0：侵入式 LRU 链表 + 节点复用 + 专用堆

**问题**：
- `container/list` 的 `Element` 是第三块分配（node、element、map bucket），且 `MoveToFront` 要经过 `element` 间接寻址。
- 每次 `Set` 新 key 都分配 `node`；在 `InsertEvict`/scan 下形成持续 GC 压力（55 B/op）。
- `time.Time` 占 24B，比较要走 wall + monotonic 两段；`container/heap` 的 `Less/Swap/Push/Pop` 是接口分派。
- `expiryIndex int` 在 64 位上是 8B；1M entry 的堆数组额外 8MB，但索引本身必要。

**实测（fast-s1，单线程）**：

| 指标 | base | fast-s1 |
|---|---:|---:|
| `Lookup` | 18.7 | **15.7** |
| `Set`（在 notime 基础上） | 23.6 | **18.5** |
| `InsertEvict` | 88.3（notime） | **33.2** |
| `InsertEvict` 分配 | 55 B/op | **0 B/op** |
| 每 entry live heap | 147.3 B | **99.0 B** |
| `RenewTTL` | 146.8（earlier） | **72.1** |

**实现要点**：

1. `entry` 内嵌 `prev/next`，`shard.head/tail` 直接维护；`moveFront` 先判断 `head == e`（热 key 连续命中时省掉 6 次指针写）。
2. 自由链表 `free`：淘汰/删除时把节点清空（`key=""`、`value=zero`、`deadline=0`、`heapIndex=-1`）后挂回；新插入优先复用。**必须清空 value**，否则 freelist 会延长大对象的生命周期；`Clear/Close` 要丢弃 freelist。
3. `deadline int64`：构造时记 `start time.Time`（带 monotonic），`now := int64(time.Since(start))`，deadline = now + ttl。**不要用 `UnixNano()`**，否则墙钟跳变会破坏 TTL 语义。超过 `maxTTLSeconds` 时 clamp。
4. 专用 `expiryHeap`：`less/swap/up/down/push/removeAt/fix` 全部具体方法，避免 `container/heap` 的接口分派；`removeAt` 同时清除被删元素的 `heapIndex`（这是原型里实际踩到的 bug：只清“最后一个元素”会留下 stale index，后续 `removeAt(-1)` 直接 panic）。
5. map 预分配：`make(map[string]*entry, cap(shard))` 能消掉增长抖动，但 1M 容量会一次性占 ~40MB；建议**按分片预分配**，或只对 `capacity <= 64K` 预分配，或做成 `Preallocate` 选项。

**风险**：中。纯内部改动，但涉及 LRU/堆/复用的不变量。必须有结构不变量测试（map/list/heap 一致、每个 expiring entry 恰好一条堆记录、freelist 不残留 key/value）和 fuzz。

**验收**：
- `Lookup`/`Set` 0 allocs/op；
- `InsertEvict` 0 B/op；
- 1K 容量 footprint ≤110 B/entry；
- `RenewTTL` ≥30% 改善；
- 既有 `make check`、模型 fuzz、shuffled race 全绿。

---

### 3.3 P1：`Size()` 的清理不应在全局锁内完成

**问题**：`Size()` 为了返回“精确 live 数”，在锁内 `purgeExpiredLocked(now)` 清空所有过期项。100K 过期项 7.9ms，1M 时仓库 Stage A 158.5ms / B2 175ms；期间所有读写都被阻塞，open-loop 的 `size-burst` p99 达到 20ms 量级。

**实测**：

| 场景（100K 过期） | `Size()` 总耗时 | 并发 `Lookup` 最大阻塞 |
|---|---:|---:|
| base（单锁） | 7.9ms | **10.9ms** |
| fast-s16（16 分片） | 11.8ms | **0.68ms** |

分片只降低“最大持锁时间”，不降低总工作量；总时间甚至因 16 次 lock/unlock 略升。

**可选方案（按侵入性排序）**：

1. **新增 `Len() uint`**：O(1) 返回 `len(holder)`（物理条数，可能含已过期），`Size()` 保持精确但文档标注代价。这是最小 API 变更，建议先做。
2. **`Size()` 分批清理**：每次最多清 N 条（如 1024），返回当前 live 数；语义变成“最多清理 N 条后的精确值”，需要明确文档/版本。
3. **清理移出锁**：锁内把过期节点摘到临时切片，锁外释放 value；但摘除本身仍需锁，只能缩短“释放大对象”的时间。
4. **异步清理 + 读时严格**：`Size()` 只返回计数并唤醒 cleaner；过期读仍严格 miss。语义最松，适合监控场景。
5. **精确 live 计数**：维护 `expiredCount`，但条目是随时间连续过期的，没法在不扫描的情况下保持精确；不推荐。

**建议**：Stage 1 先加 `Len()` 并在 `Size()` 文档中写明 O(k log n) 与阻塞风险；Stage 3 再决定是否改 `Size()` 语义（需要 major version）。

**验收**：1M 容量 `Size()` 不再出现 >10ms 的单次停顿；`Len()` 在 1M 下 <100ns；并发 `Lookup` 的 p99 不受 `Size()` 影响。

---

### 3.4 P1：cleaner 懒启动

**问题**：`WithLRU(cap, true)` 在容量 >0 时无条件 `go s.runCleaner()`，即使应用从不设置 TTL。1000 个这样的实例 = 1000 个 goroutine + 1.31MB（1.35KB/实例），外加每个实例的 timer/唤醒路径。

**实测**：`TestIdleInstances` 1000 个 TTL 缓存（无 deadline）：goroutine +1000，heap +1.31MB。这与仓库 B2 报告的 1.6–2.3MB/1000 实例同量级。

**实现**：
- 构造时只创建 `wake`/`done`/`stopped`，不启动 worker；
- 第一次“真正写入 TTL”（`expiring && ttl > 0`）时，在锁内置 `started=true` 并 `go s.runCleaner()`（只启动一次）；
- `Close()` 若 `started == false`，直接 `close(s.stopped)` 再返回，避免 `<-s.stopped` 永久阻塞；
- worker 启动后仍保持现有“空堆无 timer”的行为。

**风险**：低，但要处理 `Close` 与首次 `SetExpire` 的竞态（锁内 `closed/started` 双标志），以及 `startCleaner=false` 的测试路径。

**收益**：不设 TTL 的 TTL 缓存从 1 goroutine/1.35KB 降到 0；常见“先开 TTL 能力、后按需用”的配置直接受益。

---

### 3.5 P1：清理批大小自适应 / 时间盒

**问题**：固定 64/batch。100K 过期项需要 1562 次“lock → 清 64 → unlock → Gosched”；1M 需要 15625 次。总回收时间在 B2 细粒度测量里是基线的 1.52x，属于可接受但可优化。

**建议**：
- 按堆规模自适应：`batch = clamp(len(heap)/64, 64, 4096)`；或
- 时间盒：每批最多持锁 100–250µs，按实际清理速度动态调整；
- 批间 `runtime.Gosched()` 改为“每 N 批一次”或只在检测到等待者时让出；
- 保持“单批不扫描全堆、只取堆根”的现有性质。

**验收**：100K/1M 回收时间 ≤1.2x 基线；expiry-burst 的 Set p99 不回退；单次持锁时间 <1ms。

---

### 3.6 P2：仅“更早 root”唤醒——实测不值得

B2 的设计评审输入 #2 认为“每次 root 变化都唤醒”导致 `RenewTTL` 回退 10–50%。本轮 `earlier` 变体（只在 root 变早/变空/从空变有时唤醒）在 1K 循环下只从 152.3ns 降到 146.8ns（**−3.6%**），因为：
- 非阻塞 one-slot send 本身很便宜；
- 该循环的 root 大多在“变晚”，本来就不需要立刻唤醒；
- 主要成本在堆更新，不在唤醒。

**结论**：不要把“唤醒契约”作为独立优化项；如果未来 TTL 续约成为瓶颈，优先换 int64 + 专用堆/时间轮。若仍要改，必须补 `Clear/Delete` 的 promptness 语义测试。

---

### 3.7 P2：map 预分配与参数

- 容量已知，`make(map, capacity)` 可避免增长时的 rehash 停顿（1M 容量下 `InsertEvict` 417ns vs 1K 112ns，部分来自 map 增长/缓存）。
- 但 1M 容量一次性预分配约 40MB；小缓存预分配又浪费。建议：
  - 只在 `capacity <= 64K` 时预分配；
  - 或暴露 `Preallocate(bool)` / `InitialSize(uint)`；
  - 分片后按 `cap/shards` 预分配，天然摊薄。
- Go 1.24+ 的 Swiss map 会自动改善查找/内存，无需改代码；但库的最低版本是 1.18，不能依赖。

---

### 3.8 P3：热路径的 `defer` 与 `runtime.KeepAlive`

- `set/lookup/delete` 用 `defer s.mu.Unlock()`；Go 1.18 会 open-code，成本约 1–2ns。对 15ns 的 `Lookup` 是 7–10%，可以改手动 unlock，但收益小、易错（panic 路径）。
- `notifyCleanerLocked` 的 `defer` 在 `wake != nil` 时每次写都注册；可在堆为空时直接跳过（见 3.1 的 `needNow` 条件顺带优化）。
- `runtime.KeepAlive(m)` 是编译器屏障，成本接近 0，不需要动。

**建议**：只在做 3.1/3.2 时顺手把 `set` 的 defer 改成“成功路径手动 unlock、panic 路径不保证”（库内无 panic 点），并用 benchmark 验证 ≥3% 再保留。

---

### 3.9 P2：`Get` 的 1 次分配

`Get` 返回 `&value` 必然逃逸，1 alloc/op（8B）。这是文档化的 API 契约，不建议改内部指针（别名风险）。已经在文档中引导热路径用 `Lookup`；可以：
- 增加 `GetInto(dst *T) bool` 或批量 API，避免每 key 一次指针分配；
- 或在 `Stats` 中暴露 alloc 相关指标，提醒使用者。

---

### 3.10 P2：注册表、finalizer、`Clear`/`Close`

- **注册表**：`lives` 是全局 `sync.RWMutex + map`，创建/关闭串行。高 churn 场景可换 `sync.Map` + `atomic.Int64` 计数（Go 1.18 用 `atomic.AddInt64`），`CLEAR_ALL` 用 range 快照。
- **finalizer**：每个实例一个 `SetFinalizer` 闭包；短生命周期实例会增加 finalizer 队列与一次额外 GC 周期。可做成 `WithoutFinalizer()` 选项，或只在显式 `Close` 不可用的场景启用。先测量目标应用的 GC 开销再决定。
- **`Clear`**：`make(map)` 换新 map，旧 map 等 GC；1M 时新旧两份 map 可能同时存活（瞬时 ~2x）。可改为“锁内换指针、锁外异步释放”，或对小 map 用原地 delete。
- **`CLEAR_ALL`**：串行清理所有缓存；可限制并发度（如 `min(GOMAXPROCS, len)`）并行清理，缩短总时间，但每实例仍各自持锁。
- **`Close`**：等待 cleaner 退出；懒启动后要保证未启动时也能立即返回（见 3.4）。

---

## 4. 架构级优化

### 4.1 分片（Sharding）：必要，但不是银弹

**设计**：

```
Cache
 ├─ shards [N]*shard        // N = 2^k
 │   ├─ mu sync.Mutex       // 每分片独立，缓存行对齐
 │   ├─ m map[string]*entry
 │   ├─ head/tail *entry    // 侵入式 LRU
 │   ├─ heap []*entry       // 每分片独立过期堆
 │   └─ free *entry         // 每分片 freelist
 ├─ mask uint64
 ├─ capacity / ttl / start
 └─ one cleaner (跨分片取 min root)
```

- **路由**：`h := seededHash(key); shard := shards[h & mask]`。FNV-1a 的低位对本仓库的 key 分布够用（1024 key 分到 64 片，无空片），但**低位雪崩弱、可被构造冲突**；建议用带种子的 64-bit 混合（如 wyhash 风格）或 `hash/maphash`（注意 Go 1.18 的 string 路径便利性不如后续版本，可能要自定义以避免 string→[]byte 分配）。
- **容量切分**：`base = capacity/N`，余数分给前 `capacity%N` 片；总容量精确等于 `capacity`。不要“每片 capacity/N + 1”，那会超配。
- **分片数**：`N` 取 2 的幂；`N <= capacity/64`（每片至少 ~64 槽，否则容量损失和 hash 抖动明显）；默认 **1**（保持精确 LRU 兼容），通过 `Shards(n)` 显式开启。经验值：8–16 片；24 线程可试 32–64，但收益递减。
- **TTL**：每片一个堆；一个 cleaner 读所有片的 root 取 min，arm 一个 timer；到期后逐片、按时间盒清理。`Size()` 逐片清理后求和；`Clear/Close` 逐片加锁（一次只持一把，避免死锁）。
- **缓存行隔离**：分片结构体要把 `mu` 和 `head/tail/m/heap` 放在不同缓存行，并避免相邻分片共享同一行。原型用 `_ [56]byte` + 尾部 padding，但 Go 分配器不保证 64B 对齐；生产实现应在分配时按 64B 对齐（或每片单独 mmap/大对象对齐），并用 `go test -bench` 对比“有/无 padding”确认。
- **注册表**：只注册一个 state，不按分片注册；`CLEAR_ALL` 仍是一个实例一次调用。
- **Stats**：每片普通计数器（锁内 `uint64++`），`Stats()` 逐片加锁求和；不要用全局 atomic。

**实测收益（24 线程，共享 key）**：

| 指标 | base | s16 | s64 |
|---|---:|---:|---:|
| `Lookup` Mops/s | 12.1 | 29.7 | **71.4** |
| `Set` Mops/s | 11.8 | 25.3 | **55.9** |
| 单线程 `Lookup` ns | 18.7 | 17.7 | 18.6 |
| 全驻留命中率 | 100% | 98.2% | 94.3% |
| Zipf 命中率 | 79.65% | 79.65% | 79.58% |

**代价**：
- **近似 LRU**：全局 LRU 变成“每片 LRU”，跨片热点无法互相保护；全驻留均匀工作集损失 1.8%（16 片）/5.7%（64 片）。倾斜负载（Zipf）几乎无损。
- **容量不均**：hash 分布导致某些片先满，实际有效容量略低；可用“片间借容量”（复杂、需跨片锁）或“选择最少使用的片淘汰”（会引入跨片同步）缓解，不建议 v1 做。
- **TTL 调度复杂度**：这是分片最难的部分；如果短期不想动 TTL，可先只对 `enableExpired=false` 的缓存开放分片，TTL 缓存回退单分片，等调度器完成后再统一。
- **单线程回退**：分片增加 hash + 间接寻址，单线程 `Set` 从 18.5 退到 30ns（+62%）；对延迟敏感的串行调用方要能接受，或按容量/负载自动选 1 片。

**验收门槛**：
- 8 线程 `Lookup`/`Set` ≥3x base；24 线程 ≥4x base（不要求线性）；
- Zipf 命中率损失 ≤2%；全驻留损失 ≤3%（16 片）；
- 单线程 `Lookup` 回退 ≤10%；`Set` 回退 ≤70%（分片模式的 hash/间接成本固有，单线程不是它的目标场景）；
- TTL 语义、`Size/Clear/Close`、race/fuzz 全绿；
- 分片数、hash 函数、容量切分写入文档。

---

### 4.2 读路径：精确 LRU 的命中是写操作

这是比“分片”更根本的架构问题。

**证据**：

| 读路径 | 24 线程 Mops/s | 说明 |
|---|---:|---|
| `Lookup`（命中 `MoveToFront`）base | 12.1 | 单锁 + 写 LRU |
| `Lookup` fast-s64 | 71.4 | 分片后仍写 LRU |
| `Peek`（不写 recency）fast-s64 | **213** | 分片 + 不写 |
| 无锁只读 map | **1388** | 没有锁/写共享状态 |
| 私有 mutex lock/unlock | **5060** | 缓存行私有的上限 |

**结论**：只要每次命中都要 lock/unlock 并改 `prev/next`，多核吞吐就会被“共享 mutex 行 + entry/list 写”限制；分片只能把冲突从“一把锁”分散到“多把锁”，不能消除写共享。`Peek` 去掉 list 写能拿回约 3x，但仍有 mutex 上限；真正接近线性需要无锁读（不可变快照/`sync.Map` 读索引）。

**可选方案**：

1. **新增 `Peek(key) (T, bool)`**：不刷新 LRU，过期仍 miss。语义清晰，适合“读多、可接受近似 LRU”的场景；实现上可先用每片 `RWMutex`（读多时比 Mutex 好），或每片 `sync.Map`（读路径接近无锁）。
   - 实测：`Peek` 在 64 片下 213 Mops/s，是 `Lookup` 的 3x；但仍是 mutex，未到无锁 map 的 1.4B。
2. **`sync.Map` 作为读索引**：Set/Delete 同时更新 `sync.Map` 与 LRU 链表（链表仍由分片锁保护）；`Peek` 只 `Load`，读路径无锁。代价：写更贵、内存翻倍（map 两份），需要仔细处理一致性。
3. **RCU/不可变快照**：每片维护 `atomic.Pointer[snapshot]`，读 `Load` 后查不可变 map；写复制该片 map（O(n)）后原子替换。只适合“读远多于写 + 分片足够小”；否则写放大严重。
4. **CLOCK / 二次机会**：每 entry 一个 atomic ref bit，读只置位（仍是写，但可批量/采样）；淘汰扫描。能避免链表指针写，但 atomic 位仍有缓存行写；适合近似 LRU。
5. **采样 LRU / SLRU / 2Q**：不维护精确全序，命中只更新概率/分段；可显著减少写，但命中率模型改变。

**建议**：先做 `Peek`（低风险、可独立发布），再根据目标场景决定是否引入 `sync.Map`/RCU。**不要**把 `Lookup` 直接改成 `RWMutex + 不移动`——那会悄悄把 LRU 变成 FIFO，是语义破坏。

---

### 4.3 TTL 调度架构

现状（B2 后）已经不错：每实例一个 worker、空堆无 timer、最早 deadline、批量 64、one-slot 唤醒。剩下的是“规模”和“单位成本”。

**选项 A：懒启动 + 自适应批 + 专用堆（推荐先做）**
- 成本最低，收益确定（见 3.2/3.4/3.5）；
- 保持每实例隔离，无全局调度器；
- 适合实例数 < 几千。

**选项 B：共享调度器 / 时间轮（实例数很大时）**
- 一个进程级 goroutine（或按 P 分片）维护所有缓存的 deadline；
- O(1) 插入/取消（分层时间轮），避免每实例一个 goroutine；
- 代价：全局状态、公平性、关停顺序、跨缓存隔离；一个慢缓存可能拖累其他缓存；
- 建议做成 opt-in（`WithSharedScheduler`），默认仍每实例一个。

**选项 C：时间轮替代堆**
- TTL API 是秒级，天然适合 1s 桶；分层轮（秒/分/时）可覆盖长 TTL；
- 每个桶内双向链表 + entry 指针，删除 O(1)；
- 代价：精度固定（秒级已够）、长 TTL 需要层级、取消要维护指针、`Size()` 仍需逐桶清理；
- 只有在 TTL 操作占比很高（如 >1M TTL ops/s）时才值得。

**选项 D：粗粒度时钟缓存（谨慎）**
- TTL 命中 50ns，其中 ~30ns 是时钟；如果维护一个“最多滞后 δ 的 coarse now”，且只在 `deadline > coarseNow + δ` 时跳过精确读，可保持正确性（因为 `coarseNow + δ >= real now`）。
- 需要定期更新 coarseNow；空闲时可能过期，必须回退精确读。
- 复杂度/收益比一般，除非 TTL 读占主导。**不建议 v1 做。**

---

### 4.4 淘汰策略：LRU 的 scan 脆弱性

**证据**：Scan50（4x key 池、50% 读、持续 eviction）单线程：
- base 83.3ns，fast-s1 27.3ns（快是因为 0 分配/链表），但**命中率仍然接近 0**；
- LRU 对顺序扫描没有抵抗力：扫描会把所有热 key 挤掉。

**可选策略**：

| 策略 | 优点 | 代价 | 适用 |
|---|---|---|---|
| 2Q / SLRU | 实现简单，scan 抵抗明显 | 两段容量调参 | 通用 |
| S3-FIFO | 低开销、scan 抵抗好 | 三段队列，命中移动 | 写多/scan |
| W-TinyLFU | 频率准入，命中率高 | sketch + 准入判断，CPU/内存 | 倾斜读多 |
| ARC | 自适应 | 复杂度较高 | 通用 |

**建议**：不要直接换默认策略。先做**离线模拟**：用仓库 `workload_test.go` 的 uniform/zipf/scan 生成器，在 1K/100K/1M 容量下比较 LRU/2Q/SLRU/S3-FIFO 的命中率与每次操作成本；确认目标 workload 有 ≥5% 命中率收益，再考虑内部 `policy` 接口 + opt-in。精确 LRU 是当前 README 的契约，换策略是 major change。

---

### 4.5 容量语义：entry 数 ≠ 内存

当前 `capacity` 只限制条目数，不限制字节。1M entry × 4KB payload = 4GB，库无法阻止。**这是架构级的内存风险**。

**建议**：
- 增加 `Weigher[T] func(key string, value T) int64` + `MaxBytes int64`（或 `Config.Weight`）；
- 淘汰循环可能一次需要淘汰多条才能腾出空间 → 必须时间盒，避免单次 Set 长时间持锁；
- 文档明确“key/value 的估算成本”和“引用类型不复制”的语义；
- 与分片结合：每片独立字节预算，或全局预算 + 近似。

---

### 4.6 API 层：为性能而设计的接口

| API | 目的 | 备注 |
|---|---|---|
| `Peek(key) (T, bool)` | 不刷新 LRU 的读 | 读扩展的关键；语义需文档 |
| `Len() uint` | O(1) 物理条数 | 替代监控场景的 `Size()` |
| `GetMulti(keys []string) []...` | 摊薄锁/堆开销 | 分片后按片分组 |
| `SetMulti(items ...)` | 批量写 | 同上 |
| `SetExpireDuration(key, value, time.Duration)` | 亚秒 TTL | 当前只有秒级 int64 |
| `Stats() Stats` | hits/misses/evictions/expired/entries/bytes | 每片锁内计数 |
| `WithOptions(Config)` | Shards/Weigher/Policy/Preallocate/CleanerBatch | 保持 `WithLRU` 兼容 |

**注意**：批量 API 不要在持有锁时调用用户回调；返回副本或写入调用方提供的切片。`GetMulti` 的返回值分配要可预算（预分配结果切片）。

---

### 4.7 可插拔的内部 seam

现在已有 `cacheClock` seam，这是很好的基础。建议把以下也做成**私有**接口/函数字段，便于 A/B 与后续演进，但不要暴露为公共 API：

- `policy`（LRU / 2Q / S3-FIFO）；
- `hasher`（分片路由，带种子）；
- `weigher`（字节容量）；
- `scheduler`（每实例 / 共享）；
- `statsSink`（可选）。

这样每个优化都能在同一 harness 下 A/B，而不是 fork 整个库。

---

## 5. 路线图与验收门槛

### Stage 1：零风险单点（预计 1 周）

1. 惰性时钟（3.1）。
2. 侵入式链表 + freelist + int64 deadline + 专用堆（3.2）。
3. 补齐：`Len()`、分配断言、footprint 测试、结构不变量测试。
4. 顺手：`set` 的 defer 手动化（若 ≥3% 再保留）。

**Gate**：
- 非 TTL `Set` ≤25ns（本机），TTL 无 deadline `Set` ≤35ns；
- `InsertEvict` ≤40ns、0 B/op；
- footprint ≤110 B/entry；
- `Lookup` 回退 ≤3%、`RenewTTL` ≥30% 改善；
- `make check`、`make fuzz`、`make perf-compare` 全绿；
- Go 1.18/1.27 都跑 race。

### Stage 2：分片 + 读路径（预计 1–2 周）

1. `Config.Shards`（默认 1，opt-in）。
2. 每片状态 + 跨片 cleaner（或先只对非 TTL 开放）。
3. `Peek`（每片 RWMutex 或 `sync.Map`，二选一先做小的）。
4. `Size/Clear/Close` 的分片语义与测试。
5. 高核数 benchmark（`-cpu=1,2,4,8,16,24`）+ AB/BA 交错。

**Gate**：
- 8 线程 `Lookup`/`Set` ≥3x base；24 线程 ≥4x base；
- 单线程 `Lookup` 回退 ≤10%；`Set` 回退 ≤70%（分片模式的固有成本，单线程不是目标场景）；
- Zipf 命中率损失 ≤2%，全驻留 ≤3%（16 片）；
- `Peek` 24 线程 ≥150 Mops/s；
- TTL/expiry/Size 语义不变，race/fuzz 全绿。

### Stage 3：TTL 与尾部延迟（预计 1 周）

1. cleaner 懒启动（3.4）。
2. 自适应批/时间盒（3.5）。
3. `Size()` 语义决策：`Len()` + 分批/异步（3.3）。
4. 共享调度器/时间轮：只做设计评审，不默认开启。

**Gate**：
- 1000 个未用 TTL 实例：0 goroutine、仅状态对象 ≤0.5MB；
- 100K/1M 回收 ≤1.2x 基线；
- `Size()` 最大阻塞 <1ms（1M，通过分批/异步实现，不依赖分片）；
- expiry-burst p99 不回退。

### Stage 4：策略与容量（预计 2 周+，先模拟）

1. 离线模拟 2Q/SLRU/S3-FIFO/TinyLFU。
2. `Weigher`/字节容量。
3. `Stats`、批量 API、注册表/finalizer。
4. 只有在模拟有 ≥5% 命中率收益时才引入 policy。

**Gate**：
- 模拟报告（命中率/CPU/内存）先于实现；
- 新策略 opt-in，默认仍 LRU；
- 字节容量下 1M×4KB 不再无界；
- `Stats` 热路径开销 ≤2ns。

### 5.5 测试与基准的缺口

这些不是运行时优化，但会决定优化是否可验证：

- **高核数没有门禁**：`Makefile` / `perf-baseline.sh` 的 micro 矩阵最高 `-cpu=8`，而 24 线程才暴露负扩展。建议至少在家里/CI 的高核 runner 上跑 `-cpu=1,2,4,8,16,24`，并用 Mops/s 而不是只看 ns/op。
- **`testing.PB.Next` 的版本差异**：`BenchmarkCache/ParallelLookup` 用 `b.RunParallel`。Go 1.18 的 `pb.Next()` 是每次迭代一个共享原子计数器；新版本改为按 grain 批量领取（本机 Go 1.27 已无此瓶颈）。因此高核数对比不能假设“两边的 harness 上限一样”，建议用“每 worker 私有迭代计数”的自定义并行 harness（见 `.perf-lab` 的 `BenchmarkScaling`），至少保证 Go 1.18 与 stable 的口径一致。
- **没有分配断言**：现在只有 `-benchmem` 输出，没有 `testing.AllocsPerRun` 测试。建议把 `Lookup`/`Set` 0 allocs、`Get` 1 alloc 写成测试，防止回归。
- **没有命中率断言**：`workload_test.go` 只保证“scan 仍在 evict”，没有“Zipf/scan 命中率不低于 LRU 基线”的测试；换策略/分片时容易悄悄退化。
- **没有 footprint 预算**：建议把 `B/entry` 作为 perf 报告的一等指标（原型 `TestFootprint` 可直接移植）。
- **没有 AB/BA 交错**：高核数微基准的 run-to-run 方差很大，单次 A 后 B 的对比会把频率/热漂移当成优化；至少用 AB/BA 轮换 + 中位数。
- **profile 没进门禁**：`cpu/mutex/heap` profile 只在 Stage A 手工采集；建议在 `perf-baseline` 里保留，并对 mutex delay 设阈值。

---

## 6. 不建议做的事

1. **不要只把 `Lookup` 换成 `RWMutex` 读锁**：命中仍要 `MoveToFront`，读锁下改链表是数据竞争；去掉移动就把 LRU 变成 FIFO。
2. **不要为 `RenewTTL` 改唤醒契约**：本轮实测只有 3–4% 收益，收益不足以承担 `Clear/Delete` promptness 的语义变化。
3. **不要默认开启分片**：精确 LRU/容量语义会变；小容量（<1024）分片会明显损失命中率。
4. **不要用墙钟 `UnixNano()` 存 deadline**：系统时间跳变会破坏 TTL；用 `time.Since(start)` 的 monotonic 纳秒。
5. **不要一次性给 1M 容量预分配完整 map**：40MB 起步，且可能长期闲置；按分片/上限预分配。
6. **不要用全局 `sync.Pool` 复用节点**：跨缓存保留大 value、增加 GC 扫描；每缓存/每分片 freelist 更可控。
7. **不要在没有时间盒的情况下增大 cleaner batch**：会把回收停顿重新拉回前台。
8. **不要在没有 trace 模拟的情况下引入 TinyLFU**：频率 sketch 的 CPU/内存成本可能吃掉命中率收益。
9. **不要为了 `Get` 的 1 次分配返回内部指针**：别名/生命周期风险；用 `Lookup`/批量 API。
10. **不要在 8 线程基线上宣称“分片可线性扩展”**：24 线程数据已经证伪；按目标 GOMAXPROCS 给范围。

---

## 7. 复现方式

所有原型在 `.perf-lab/`（未提交，Go 工具链忽略）：

```sh
cd .perf-lab
export GOCACHE="$PWD/.gocache"   # 沙箱环境需要可写缓存

go test -run '^$' -bench '^BenchmarkImpls$' -benchmem \
  -benchtime=300ms -count=2 -cpu=1,2,4,8,16,24 .

go test -run '^$' -bench '^BenchmarkScaling$' -benchtime=150ms \
  -count=2 -cpu=1,2,4,8,12,16,24 .

go test -run 'TestSizePurgeStall|TestSizePurgeBlocksReaders|TestIdleInstances' -v .
```

原始结果：`out-core.txt`、`out-stag.txt`、`out-scal2.txt`、`out-lock.txt`、`out-mix.txt`、`out-ttl.txt`。

**注意**：
- `.perf-lab/fast` 是原型，不是生产代码；它没有 cleaner、没有 finalizer/registry，`Size/Clear/Close` 也只实现了测量所需部分。
- 原型在 `removeAt` 上踩过一个真实 bug：删除非末尾元素时没有清除被删元素的 `heapIndex`，导致后续 `removeAt(-1)` panic；`TestFastSetAfterTTL` + `CheckHeap` 是为此加的。若把 3.2 落地，必须带等价的不变量测试。
- 这台机器是 12 物理核 / 24 SMT；高核数方差较大，报告只对方向和量级负责。

---

## 附录 A：完整单线程数据（cpu=1，ns/op）

| 操作 | base | notime | earlier | fast-s1 | fast-s8 | fast-s16 | fast-s64 |
|---|---:|---:|---:|---:|---:|---:|---:|
| Lookup | 18.66 | 18.20 | 18.81 | 15.71 | 17.34 | 17.73 | 18.57 |
| Set | 54.41 | 23.62 | 26.09 | 18.47 | 29.58 | 31.12 | 29.95 |
| InsertEvict | 119.15 | 88.27 | 95.64 | 33.22 | 34.98 | 34.78 | 34.79 |
| RenewTTL | 152.30 | 150.35 | 146.80 | 72.14 | 81.20 | 78.14 | 66.84 |
| TTL Lookup | 50.57 | 50.81 | 50.71 | 34.21 | 36.46 | 36.58 | 37.45 |

`InsertEvict` 的 B/op：base/notime/earlier = 55；fast 全系 = 0。

## 附录 B：完整并行数据（Mops/s，自定义 harness，共享 key）

| 实现 | 1 | 2 | 4 | 8 | 12 | 16 | 24 |
|---|---:|---:|---:|---:|---:|---:|---:|
| base Lookup | 55.8 | 41.0 | 32.1 | 28.1 | 22.7 | 13.9 | 12.1 |
| s16 Lookup | 57.1 | 47.6 | 50.9 | 49.7 | 46.6 | 28.6 | 29.7 |
| s64 Lookup | 55.6 | 62.6 | 88.7 | 88.2 | 62.8 | 67.0 | 71.4 |
| base Set | 18.3 | 17.4 | 14.8 | 12.6 | 12.5 | 13.2 | 11.8 |
| s16 Set | 32.8 | 29.3 | 36.2 | 36.5 | 28.5 | 23.6 | 25.3 |
| s64 Set | 32.5 | 31.5 | 54.8 | 69.2 | 49.4 | 50.3 | 55.9 |

## 附录 C：读路径对照（24 线程）

| 路径 | Mops/s | 备注 |
|---|---:|---|
| base `Lookup`（单锁 + 写 LRU） | 12.1 | 负扩展 |
| base-rw `Peek`（单 RWMutex，不写） | 23 | 读锁同一缓存行 |
| fast-s64 `Peek`（分片 Mutex，不写） | 213 | 2–3x `Lookup` |
| 无锁只读 map | 1388 | 上限参考 |
| 私有 mutex lock/unlock | 5060 | 缓存行私有的上限 |
| 共享 mutex lock/unlock（64 片） | ~250–360 | 共享缓存行上限 |

---

**一句话总结**：先做“不读不需要的时钟、不分配不需要的节点、不维护不需要的 `time.Time`/`list.Element`”这三个零/低风险单点，能拿到 **Set −57%、InsertEvict −72%（相对 base）、内存 −33%、RenewTTL −53%**；再用“opt-in 分片 + `Peek` + TTL 懒启动/时间盒”解决 8–24 线程的扩展与尾延迟；精确 LRU 的多核线性扩展需要读路径不再写共享状态，这是后续最大的架构决策，而不是再加几把锁。
