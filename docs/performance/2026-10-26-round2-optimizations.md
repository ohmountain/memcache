# memcache 第二轮性能优化：时钟、分片哈希与统计开关

- **日期**：2026-10-26
- **对象**：`github.com/ohmountain/memcache`（Go 1.18+，零外部依赖）
- **测量环境**：Linux/amd64，AMD Ryzen 9 7900X（12 物理核 / 24 线程），Go 1.27.2；测量时机器空闲，单进程串行执行
- **对照方式**：`git archive HEAD`（`afc35b1`）导出到 `.perf-lab/baseline-head`，与工作区使用**同一份**公共 API benchmark，同一 GOCACHE、同一 CPU 配置；单线程数字取 3 次（公共 API harness）或 7 次（仓库 `BenchmarkCache`）的中位数
- **状态**：三项改动已落地并通过 `make check`（gofmt、golangci-lint 2.15.0、vet、测试、3 轮 shuffled race）；本文 §5 的机会经过测量但**未实施**
- **边界**：本文只改运行时实现与新增一个 opt-out 选项；公开方法签名、默认 LRU/容量/TTL 语义不变

---

## 1. 结论摘要

1. **TTL 读路径的最大单点成本是“读墙钟”，不是 LRU。**
   `time.Now()` 在本机为 32ns，而 `time.Since(start)` 在带单调读数的 `Time` 上走单调快路径，只要 **18ns**。把 deadline 读取从 `clock.Now().Sub(start)` 改为 `clock.Since(start)`（生产时钟用 `time.Since`，测试假钟保持 `Now().Sub`）后：
   **TTL `Lookup` 49.6 → 35.8ns（−28%），TTL `Peek` 49.8 → 35.4ns（−29%），`RenewTTL` 92.0 → 79.2ns（−14%）**。

2. **分片哈希是长 key 场景的隐藏大头，已从逐字节 FNV 改为 8 字节分块混合。**
   旧实现 32B key 要 15ns、64B 要 36ns、128B 要 84ns；新实现分别为 3.6/7.4/14.4ns。端到端（64 分片、单线程 `Lookup`）：
   **8B −18%、16B −28%、32B −43%、64B −59%**。仓库原有 benchmark 用 1–4 字节数字 key，长期掩盖了这笔成本。

3. **统计计数器是可选的多核税，新增 `Options.DisableStats`。**
   计数器写的是分片里本可保持只读的 cache line；64 分片、24 线程下关闭后
   **`Lookup` −12%、`Set` −19%、`Peek` −15%**，单线程无可测量差异。默认仍开启，行为兼容。

4. **分片 `Lookup` 的多核上限仍然是“命中即写共享状态”，不是哈希。**
   24 线程 CPU profile 中 mutex 机制（lockSlow/Unlock/procyield）约 **51%**，`hashKey` 已降到 **2.1%**。64 分片 24 线程 `Lookup` 约 21–22ns，与单线程持平；`Peek` 约 9.7ns（≈103 Mops），`Peek+NoStats` 约 8.4ns（≈119 Mops）。要再上一个量级必须换读路径（§5.2），而不是继续调锁。

5. **仍未解决的两项旧门禁**：每实例内存 793B（非 TTL）/904B（TTL 空闲），高于 500B/1000 实例目标；分片高核数吞吐未线性。§5 给出可执行的下一步与预估收益。

### 实施项一览

| ID | 改动 | 实测收益 | 风险 |
|---|---|---|---|
| GO-PERF-R2-01 | deadline 读取走 `time.Since` 单调快路径 | TTL 读 −28%、续约 −14% | 低（语义等价，假钟保持确定性） |
| GO-PERF-R2-02 | 分块种子哈希替代逐字节 FNV | 分片读 16B −28% / 32B −43% / 64B −59% | 低（仅影响分片路由，分布有测试） |
| GO-PERF-R2-03 | `Options.DisableStats`（默认关闭该选项=统计开启） | 24 线程 64 分片：Lookup −12%、Set −19%、Peek −15% | 低（opt-in；关闭后计数为 0） |

---

## 2. 方法与口径

- 对照代码：`git archive HEAD | tar -x -C .perf-lab/baseline-head`；工作区与对照都通过 `replace` 被同一份 `.perf-lab/lead*/api_test.go` 公共 API benchmark 驱动。
- 仓库内 benchmark（`BenchmarkCache`）在两边分别执行，`-cpu=1`、`-benchtime=250ms`、`-count=7`，取中位数。
- 公共 API benchmark（key 长度、TTL 读）`-cpu=1`、`-benchtime=200ms`、`-count=3`，取中位数。
- 统计开关 A/B 用同一工作区、同一 harness，`-cpu=1,24`、`-count=3`。
- 高核数方差大：同一实现 24 线程 run-to-run 可差 10–20%；本文只对 **>10% 且方向可解释**的差异下结论，其余标注为噪声。
- `.perf-lab/` 中的原型（§5）**不是生产代码**，只用于判断量级；其数字标注为“原型”。

---

## 3. 已实施改动

### 3.1 GO-PERF-R2-01：TTL 时钟读取走单调快路径

**问题**：`nowNanos()` 原来是 `int64(s.clock.Now().Sub(s.start))`。`systemClock.Now()` 调用 `time.Now()`，本机 32ns；而 TTL 命中每次都要读一次，占 49.6ns 的 **65%**。

**机制**：`time.Now()` 同时读墙钟与单调钟；`Time.Sub`/`time.Since` 在 `Time` 带单调读数时只用单调钟。现代 Go 的 `time.Since` 有显式快路径（Go 1.27 源码：`subMono(runtimeNano()-startNano, t.ext)`），实测 17–18ns。Go 1.18 上 `time.Since` 等价于 `Now().Sub`，因此**不改变最低版本兼容性**，只是老工具链拿不到这笔收益。

**实现**：
- `cacheClock` 增加 `Since(start time.Time) time.Duration`；
- `systemClock` 增加 `monotonic bool`：生产构造（`WithOptions`）置 true，走 `time.Since`；测试注入的 now 函数置 false，保持 `now().Sub(start)`；
- `fakeClock`/`countingClock`/`stepClock` 实现 `Since` 并**委托给各自的 `Now()`**，使空闲唤醒计数、时钟门控测试的语义不变；
- `nowNanos()` 改为 `int64(s.clock.Since(s.start))`。

**测量（单线程，ns/op，中位数）**：

| 路径 | HEAD | 现在 | 变化 |
|---|---:|---:|---:|
| TTL `Lookup`（1 分片） | 49.62 | 35.81 | **−27.8%** |
| TTL `Peek`（1 分片） | 49.78 | 35.42 | **−28.8%** |
| `RenewTTL`（仓库 benchmark） | 91.96 | 79.16 | **−13.9%** |
| 非 TTL `Lookup` | 15.71 | 15.32 | −2.5%（噪声量级） |

微基准对照：`time.Now().Sub(start)` 32.5–32.9ns；`time.Since(start)` 17.4–18.1ns；接口包装后的 `Now()` 32.4ns。

**正确性**：deadline 仍是“构造时单调起点 + int64 纳秒”，`Time.Sub` 与 `time.Since` 取同一个单调读数；假钟路径逐字节等价于旧实现，现有惰性时钟/空闲唤醒/边界测试全部通过。

### 3.2 GO-PERF-R2-02：分块种子哈希

**问题**：`hashKey` 逐字节 FNV-1a，再叠一次 64 位混合。微基准（本机）：

| key 长度 | 旧 FNV | 新分块 | 加速 |
|---|---:|---:|---:|
| 8B | 3.00 | 1.93 | 1.55x |
| 16B | 6.24 | 2.42 | 2.58x |
| 32B | 15.0 | 3.85 | 3.9x |
| 64B | 36.4 | 7.39 | 4.9x |
| 128B | 84.4 | 14.4 | 5.9x |

**机制**：每轮用 8 个显式字节装载组成 little-endian word（`read8`，任何工具链都不会为它分配），`h = (h ^ w) * prime; h ^= h >> 29`；尾部不足 8 字节单独混合；最后保留原有 `xor-shift / 0x9e3779b97f4a7c15 / +seed` 雪崩，保证 `hash & mask` 用到的低位充分扩散。

**端到端（64 分片，单线程 `Lookup`，同一公共 API harness）**：

| key 长度 | HEAD | 现在 | 变化 |
|---|---:|---:|---:|
| 8B | 21.59 | 17.80 | −17.6% |
| 16B | 25.58 | 18.40 | −28.1% |
| 32B | 36.25 | 20.69 | −42.9% |
| 64B | 59.81 | 24.54 | −59.0% |
| 1 分片（不哈希） | 14.4–16.8 | 14.9–16.8 | 噪声内 |

**分布质量**：新增 `TestHashKeyDistribution`（64 分片、65536 key，覆盖纯数字、长共享前缀、类 UUID），要求最忙分片 ≤ 均值 1.35 倍且无空分片；本地另用 10 万 key × 16/64/256 分片与旧 FNV 对比，`max/mean` 处于同一区间（1.01–1.19），无退化。

**语义**：分片映射本来就用每缓存随机 seed，不属于公开契约；`Shards<=1` 的默认路径不调用哈希，完全不受影响。

### 3.3 GO-PERF-R2-03：`Options.DisableStats`

**问题**：`hits/misses/peeks/...` 每个操作都写分片结构体的计数器行。对 `Peek` 这种本来只读的路径，这是唯一的共享写；高核数下它把 cache line 从“多核共享只读”变成“反复失效”。

**实现**：`Options.DisableStats`（零值 = 保持现状），`cacheState.collectStats` 在每个计数点前加一个可预测分支；`purgeExpiredLocked`/`reclaimLocked` 增加 `collectStats` 参数。关闭后 `Stats()` 只返回 `Capacity`/`Entries`，其余字段恒为 0（文档与测试已写明）。

**测量（64 分片，公共 API，`-cpu=24`，中位数）**：

| 操作 | 统计开 | 统计关 | 变化 |
|---|---:|---:|---:|
| `Lookup` | 22.36 | 19.59 | **−12.4%** |
| `Set` | 20.93 | 17.06 | **−18.5%** |
| `Peek` | 9.82 | 8.37 | **−14.8%** |

单线程（`-cpu=1`）：`Lookup` 21.26 vs 21.13、`Set` 23.82 vs 23.72、`Peek` 16.67 vs 16.82，差异在噪声内；默认路径的额外分支成本在仓库 benchmark 中为 0–2%（`Lookup` 15.71→15.32、`Set` 17.40→16.80、`Peek` 10.63→10.84）。

> 说明：审计原型曾报告关闭统计后 `Peek` 提升 49%；在真实实现上用同一公共 API 复测只有 **约 15%**。本文以真实实现为准，49% 不采信。

---

## 4. 验证

- `make check`：gofmt 干净；golangci-lint v2.15.0 **0 issues**（只读缓存目录的告警是环境限制，不是 lint 失败）；`go vet` 通过；`go test` 通过；`go test -race -shuffle=on -count=3` 通过（23.6s）。
- 新增测试：`TestDisableStatsReportsOnlyEntries`、`TestHashKeyDistribution`。
- 新增 benchmark：`BenchmarkCacheShardedPeek`（统计开/关）、`BenchmarkCacheShardedKeyLength`（8/16/32/64B key），防止后续把这两笔收益改回去。
- 既有 TTL 语义测试（惰性时钟、空闲无唤醒、边界、模型 fuzz、shuffled race）未改动即通过。
- 未执行：Go 1.18 实机（本机仅 1.27.2）、跨平台 CI、提交后的 `make perf-compare`。

---

## 5. 已验证但未实施的机会（按收益/风险排序）

### 5.1 每实例内存：793B → 约 500–600B（P1，低风险）

实测（10000 个实例，GC 后 `HeapAlloc` 差值）：非 TTL **793.1B**、TTL 空闲 **904.3B**、容量 0 **790.4B**；`Close` 后残留 0–3B。构成大致是：`cacheState` 约 200B、`shard` 224B（208B 对象 + size class）、map 头 48B、`done`/`stopped` 两个 channel 各约 104B、TTL 另加 `wake` 104B、finalizer/注册表若干。

可做的四步（彼此独立）：
1. `stopped chan` + `stopOnce` → `sync.WaitGroup`（结构体内 12B，少一次堆分配）：约 −92B；启动/关闭都在 `lifeMu` 下排序，`Add` 先于 `Wait`，语义安全。
2. `done` 懒分配：只在 `startWorkerNow`（持 `lifeMu`）创建，未启动 worker 的缓存（非 TTL、TTL 但从未写 TTL）为 0 个 channel：约 −104B。
3. `Clear` 后 map 置 nil、首次写入再 `make`（nil map 读安全）：约 −48B/实例，并省掉 `Clear` 的那次新 map 分配。
4. 每分片 freelist 从 `[]*node`（24B）改为用 `node.next` 串起来的单链表（8B），shard 体从 160B 降到 144B，补 48B 后正好 **192B = 3 条 cache line**，同时消除 224B size class 的 32B 偏移（仍保留 pad 兜底）：约 −16B/分片，且对齐更稳。

预计非 TTL 793 → 约 **500–550B**，TTL 空闲 904 → 约 **610–660B**（若再把 `wake` 用 `atomic.Value` 懒分配可到约 510–560B，但 `notifyCleaner` 的丢失唤醒证明需要仔细处理，见下）。这只把“1000 实例 ≤0.5MB”从 ❌ 拉到接近/达到，收益集中在“很多小缓存”的用法；风险集中在 `Close`/首次 TTL 写/`notifyCleaner` 的生命周期分支。**本轮未做**：改动面比三项已实施项大，且不影响热路径。

> 懒分配 `wake` 的陷阱：`notifyCleaner` 在无锁路径读 `wake`，直接写字段会构成数据竞争；必须用 `atomic.Value`（Go 1.18 可用）或把分配放在 worker 启动前的同一个锁序里，并证明“看到 nil 的 notify 之后 worker 的首次快照一定能看到该 deadline”。这个证明成立（unlock 在 notify 之前、分配在 load 之后、worker 快照在分配之后），但需要专门的竞态测试。

### 5.2 分片多核上限：锁与“命中即写”仍是主因（P1，架构级）

24 线程、64 分片 `Lookup` 的 CPU profile：

| 项 | flat% | 说明 |
|---|---:|---|
| `lookup` 本体 | 42.7 | map、拷贝、链表 |
| `procyieldAsm` | 13.7 | 自旋等待 |
| `Mutex.Unlock` | 11.8 | 含慢路径 |
| `Mutex.lockSlow` | 3.8（cum 22.6） | 竞争 |
| `mapaccess2_faststr` | 2.4（cum 6.6） | 含 AES hash |
| `hashKey` | **2.1** | 已不再是瓶颈 |
| `detach`/`moveFront` | 1.9/0.9 | 命中写链表 |

即 mutex 机制合计约 **51%**；即使把哈希降到 0，也不会改变量级。原型（`.perf-lab`，非生产，24 线程、64 分片、1024 key）对照：

| 读路径 | Mops/s | 相对生产 `Peek` |
|---|---:|---:|
| 生产 `Lookup`（写 recency） | ~52 | 0.6x |
| 生产 `Peek`（不写 recency） | ~90 | 1.0x |
| `RWMutex` 读锁 `Peek` | ~154 | 1.7x |
| 每分片 `sync.Map` 读索引 | ~571 | 6.3x |
| 每分片 RCU 快照 | ~715 | 7.9x |
| CLOCK（atomic ref bit） | ~130 | 1.4x |

代价同样明确：同一原型里 24 线程 `Set` 为 生产 55.3 Mops、`sync.Map` 16.9、RCU **2.93**；RCU 覆盖写随分片驻留量线性恶化（4 条 188ns、64 条 1.9µs、1024 条 **31µs**）。`RWMutex` 的读锁仍会写共享的 readerCount 行，且 `Peek` 的计数器在 RLock 下会数据竞争。

**结论**：RCU 不适合本库（写放大不可接受）；`sync.Map` 读索引只适合“读极多、写极少、且能接受内存翻倍”的 opt-in 模式；CLOCK/二次机会是唯一可能同时改善读写且不爆内存的方向，但会改变 LRU 语义。**下一步建议**：先做 `GetMulti`/`SetMulti` 这类批量 API（摊薄锁而不是消除锁），或在真实 trace 上验证 CLOCK 命中率损失 ≤2% 后再做 opt-in 读模式。本轮不实施。

### 5.3 淘汰策略与分片容量：opt-in 才有价值（P2，需 trace）

用独立模拟器（`.perf-lab/audit-workload/sim`，只计策略记账、cache-aside 读缺失填充）跑 LRU/2Q/SLRU/S3-FIFO/W-TinyLFU：

- 容量 1024、均匀/大工作集（≥4x）：所有策略差异 <0.2pp，LRU 足够；
- Zipf(0.8–1.2) 且工作集 ≥2x 容量：W-TinyLFU/S3-FIFO **+2～9pp**（如 zipf0.8-ws10x：LRU 43.70 → 52.94）；
- `scan50`（顺序扫描污染）：LRU **0%**，W-TinyLFU **+47pp**（ws2x）；S3-FIFO 在 `pollution90` **+4～5pp**；
- 容量 100000：收益缩小到 0–3pp（scan50 仍 +19.5pp）。

分片命中率代价（同模拟器）：容量 1024、均匀全驻留时 fixed-16 从 100% 降到 **95.79%**（−4.2pp），而“2-choice 选片”或“片间借容量”都能恢复到 100%；`scan50` 下借容量反而从 0% 提到 **23.4%**。容量 100000 时 fixed-16 有效利用率 99.6%，但 Zipf 小工作集下最忙/最闲分片差 253 条，利用率仅 64.3%。

**建议**：默认策略不动；若要引入，优先级是“分片容量借调/2-choice”（不改淘汰语义、直接补回 4pp）> S3-FIFO（pollution/scan）> W-TinyLFU（小容量 Zipf，但每次读的 sketch 记账最贵，模拟器 20–36ns/op）。所有策略都必须先在目标 trace 上验证，且 opt-in。

### 5.4 TTL 写路径：堆操作是主体，时间轮仍是最大杠杆（P2）

64 分片、24 线程 `SetExpire`（续约）profile：`set` 21.2%、自旋 17.6%、`Unlock` 10.3%、`lockSlow` cum 25.2%、**`expiryHeap` 的 less/up/down/swap 合计约 17%**、`rootDeadline` 5.7%、`nanotime` 5.5%、`deadlineSnapshot` cum 3.4%。也就是说，续约成本已经从“时钟 + 接口分派”转到“O(log n) 堆维护”。TTL API 原生是秒级，分层时间轮可以把续约/取消降到 O(1)，但需要处理精度、取消指针与内存；共享调度器（多实例一个 worker）还需先测实例数交叉点。本轮不做。

### 5.5 监控与生命周期（P2）

- `Size()` 仍是 O(k log n)：上一轮 100K 过期 6.3ms，单批持锁 1024 条。可改为“堆前缀计数 + 通知 cleaner 异步回收”（语义从“立即清理”变成“返回精确 live 数、回收延后”），或对大量 due 用 O(n) 分区 + 一次 heapify 替代逐条 `removeAt`；两者都需要先明确 `Size` 的契约。
- 创建+`Close` churn 实测 **624ns/op**；只创建不关闭则 1625ns/op（finalizer 与注册表堆积）。高 churn 场景可加 `Options` 关闭 finalizer（显式 Close 已足够），但要接受“忘记 Close 就泄漏”的风险。
- `CLEAR_ALL` 串行清理所有缓存，可限制并发度；收益取决于实例数与容量分布。

---

## 6. 明确不建议做的事

1. **不要为了 TTL 读再引入粗粒度时钟**：正确性要求“现在”的上界，任何滞后缓存都可能返回已过期值；18ns 已接近安全下限。
2. **不要默认开启 RCU/sync.Map 读索引**：RCU 写放大到 31µs/次（1024 条分片），`sync.Map` 让 `Set` 慢 3.3x 且内存翻倍；它们只适合明确的读极端场景。
3. **不要只把 `Lookup` 换 `RWMutex`**：命中仍要写链表，读锁下写链表是竞争；去掉写就把 LRU 变成 FIFO。
4. **不要在默认路径启用淘汰策略**：均匀/大容量下 LRU 已最优，换策略是 major change；先模拟再 opt-in。
5. **不要继续优化 `hashKey`**：profile 已降到 2.1%，分块版本在 128B key 上仍有 14ns 的可预测成本，但已不是瓶颈。
6. **不要用 `DisableStats` 作为默认**：统计是 `Stats()` 的契约，默认必须保留；它只是给“确认不导出计数”的高核数调用方一个开关。

---

## 7. 复现

```sh
export GOCACHE=$PWD/.perf-lab/gocache   # 沙箱/只读 home 环境需要可写缓存

# 对照导出
rm -rf .perf-lab/baseline-head
mkdir -p .perf-lab/baseline-head
git archive HEAD | tar -x -C .perf-lab/baseline-head

# 仓库内单线程对照（两边各跑一次）
(cd .perf-lab/baseline-head && go test -run '^$' -bench '^BenchmarkCache$' \
  -benchmem -benchtime=250ms -count=7 -cpu=1 .)
go test -run '^$' -bench '^BenchmarkCache$' -benchmem -benchtime=250ms -count=7 -cpu=1 .

# 公共 API 对照（key 长度 / TTL 读）
(cd .perf-lab/lead-base && go test -run '^$' \
  -bench 'BenchmarkLookupByKeyLen|BenchmarkTTLLookup|BenchmarkTTLPeek' \
  -benchmem -benchtime=200ms -count=3 -cpu=1)
(cd .perf-lab/lead && go test -run '^$' \
  -bench 'BenchmarkLookupByKeyLen|BenchmarkTTLLookup|BenchmarkTTLPeek' \
  -benchmem -benchtime=200ms -count=3 -cpu=1)

# 统计开关与高核数
go test -run '^$' -bench '^BenchmarkCacheShardedPeek$' -benchmem \
  -benchtime=200ms -count=3 -cpu=1,8,24 .
go test -run '^$' -bench '^BenchmarkCacheShardedKeyLength$' -benchmem \
  -benchtime=200ms -count=3 -cpu=1,24 .

make check
```

`.perf-lab/` 未提交（`.gitignore` 已忽略），其中的变体/模拟器只用于本文的量级判断。

---

## 8. 风险与边界

- 本文所有数字来自一台机器（12C/24T，Go 1.27.2）；高核数 run-to-run 方差 10–20%，跨机器结论需用 `make perf-compare` 复测。
- `time.Since` 快路径依赖工具链实现：Go 1.18 上仍正确但无加速；这是兼容性允许范围内的“新工具链红利”。
- 哈希改动会改变同一个 seed 下的 key→分片映射；seed 本来就每缓存随机，不构成外部契约，但依赖固定映射的调用方（不应存在）会看到行为变化。
- `DisableStats` 关闭后 `Stats()` 的计数字段恒为 0，只有 `Capacity`/`Entries` 有意义；这是显式 opt-in 的语义。
- §5 的原型数字来自本轮审计的临时副本（子代理中断前留下的 `.perf-lab/audit-*`），只用于方向判断；采纳前必须在生产实现上复测。
- 未跑：Go 1.18 实机、Linux/macOS/Windows CI 矩阵、提交后的正式 `make perf-compare`。
