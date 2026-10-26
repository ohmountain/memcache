# memcache 优化实施记录

- **日期**：2026-10-26
- **范围**：按 [优化机会评估](2026-10-26-optimization-opportunities.md) 实施 Stage 1–3 的确定性优化，并补齐低风险 API（`Len`/`Peek`/`Stats`/`SetExpireDuration`/`Options`）。
- **兼容性**：Go 1.18+，零外部依赖；`WithLRU` 默认仍是精确全局 LRU，所有既有公开方法签名不变。
- **验证**：`make check`（gofmt、golangci-lint v2.15.0、vet、单测、3 轮 shuffled race）通过，`make fuzz FUZZTIME=10s` 通过；本机 24 线程/12 物理核，Go 1.27.2。Go 1.18 实机、跨平台 CI 与提交后的 `make perf-compare` 尚未执行（见 §5）。

## 1. 已实施

| 类别 | 改动 |
|---|---|
| 写路径时钟 | 只有目标已有 deadline、本次写 TTL、或满分片且堆非空时才采样时钟 |
| 存储 | 侵入式 LRU：node 内嵌 `prev/next`（窄 value 为 56B → 64B size class）；node 按分片 freelist 复用 |
| TTL | `int64` 单调纳秒 deadline；专用 `expiryHeap`（具体方法，无 `container/heap` 接口分派）；`removeAt` 清理被删索引 |
| 分片 | `Options.Shards` opt-in；每分片独立锁/map/list/heap/freelist；容量精确切分 |
| 读路径 | 新增 `Peek`（不刷新 recency）；`Len`（物理计数，不读时钟）；`Stats` |
| `Size` | 每分片 1024 条一批清理，缩短单次持锁时间；总工作量不变 |
| cleaner | 首次真实 TTL 写入才启动；跨分片取最早 deadline；批量 64–4096 自适应；空堆无 timer |
| 注册表 | `sync.Map` + 原子计数，创建/关闭不再串行 |
| API | `WithOptions`、`Options.Preallocate`、`SetExpireDuration`、`Stats` |
| 测试 | 新增惰性时钟、惰性 cleaner、Peek/Len/Stats、分片并发/差分、Duration TTL、分批 Size、稳态分配与 footprint 预算测试 |

**刻意未做**（与评估报告一致，需要先模拟或改语义）：

- 2Q/SLRU/S3-FIFO/TinyLFU 淘汰策略（需 trace 模拟，换默认策略是 major change）；
- 时间轮/共享调度器（复杂度高，当前堆已把 `RenewTTL` 降约 1/3）；
- 字节/权重容量（需新 API 与多条目淘汰时间盒）；
- `GetMulti`/`SetMulti`（收益依赖调用方，当前不是瓶颈）；
- 异步 `Clear`、finalizer 开关、`CLEAR_ALL` 并行（P2，收益依场景）。

## 2. 基准对比

同一台机器、同一 harness；旧值为优化前从 `HEAD`（`cbba0f7`）导出的独立 checkout。单线程 1K 容量，ns/op（一次 A/B 运行；高核数与微基准的 run-to-run 波动约 ±5%）：

| 操作 | 优化前 | 优化后 | 变化 |
|---|---:|---:|---:|
| `Lookup` 命中 | 16.7 | 15.4 | **−8%** |
| `Set`（无 TTL） | 54.4 | 16.4 | **−70%** |
| `RenewTTL` | 152.3 | 93.7 | **−38%** |
| `InsertEvict` | 119 | 36.5 | **−69%** |
| `Scan50` | 88.9 | 28.8 | **−68%** |
| `Overwrite` | 61.0 | 19.4 | **−68%** |
| `InsertEvict` 分配 | 55 B/op | **0 B/op** | 稳态零分配 |
| 每 entry live heap（1K，cache-only） | 147.8 B | **100.0 B** | **−32%** |
| 每 entry live heap（100K，cache-only） | 130.9 B | **82.9 B** | **−37%** |
| 每 entry live heap（1K，+keys） | 168.7 B | **120.9 B** | **−28%** |
| 每 entry live heap（100K，+keys） | 152.2 B | **104.2 B** | **−32%** |
| `Get` | 29.7–33.6 | 30.9–33.4 | 保持 1 alloc/op（契约） |

footprint 口径：填满 `WithLRU[uint64](n, false)` 后连续两次 GC，取 `HeapAlloc` 差值除以 n。**cache-only** 在采样前预生成 key，只测缓存自身，与评估报告 `.perf-lab/bench_test.go` 的 `TestFootprint` 同口径；**+keys** 在测量区间内新建 `strconv.Itoa` key，把 key 字符串也计入。内嵌 link 后 cache-only 为 1K 100.0 B、100K 82.9 B，对 110 B 的 Gate 分别有 10 B/27 B 余量；+keys 为 120.9/104.2 B。评估报告最初预测 99.0 B，最终 82.9 B 更低，是因为 node 从 48B+24B link 的 72B 变成 56B→64B class，省掉了整个 link 分配。引用 footprint 时必须写明 cache-only 还是 +keys，两者会改变 Gate 结论。

并行（`BenchmarkCacheSharded` / `BenchmarkCache`，`b.RunParallel`）：

| 场景 | 结果 |
|---|---|
| 默认 1 分片 `ParallelLookup-8` | 32.8–38.7ns，与旧值 32–44ns 同区间（run-to-run 方差大，未声称回退） |
| `Peek`（1 分片） | 10.5ns，比 `Lookup` 少约 5ns（不写 recency） |
| 分片 64、8 线程 | 10–14ns；1 分片 33–36ns（约 2.4–3.4x） |
| 分片 64、24 线程 | 17–18ns；1 分片 55–84ns（约 3.2–4.8x，方差大） |
| 分片 16、8 线程 | 18–21ns（收益小于 64 片） |

分片是近似 LRU：均匀全驻留工作集在 16/64 片下命中率约 98.2%/94.3%（原型测量）；Zipf 倾斜负载几乎无损。`WithLRU` 默认仍为 1 片。

`Size()`（100K 过期条目）：

| 版本 | 总耗时 | 单次持锁范围 |
|---|---:|---|
| 优化前 | 7.9ms | 全部 100K |
| 优化后 | 6.3ms | 每批 1024 条/分片 |

空闲实例（1000 个 `WithLRU(cap, true)`、从不写 TTL）：

| 指标 | 优化前 | 优化后 |
|---|---:|---:|
| goroutine | +1000 | **0** |
| heap | +1.31MB | **+0.85MB** |

0.85MB 即约 886 B/实例（`cacheState` + 1 个 shard + 3 个 channel），低于旧版但高于评估报告“仅状态对象 ≤0.5MB”的门禁。

## 2.1 验收门禁对照

评估报告的 Gate 与本次实测结果。**未测**表示没有可信证据，不能按通过处理。

| 阶段 | 门禁 | 结果 | 状态 |
|---|---|---|---|
| Stage 1 | 非 TTL `Set` ≤25ns | 16.4ns | ✅ |
| Stage 1 | TTL 无 deadline `Set` ≤35ns | 未单独测 | ⚪ 未测 |
| Stage 1 | `InsertEvict` ≤40ns、0 B/op | 36.5ns、0 B/op | ✅ |
| Stage 1 | footprint ≤110 B/entry | 1K cache-only 100.0；100K 82.9 | ✅（余量 10 B；+keys 120.9/104.2） |
| Stage 1 | `Lookup` 回退 ≤3% | 16.7→15.4ns | ✅ |
| Stage 1 | `RenewTTL` ≥30% 改善 | −38% | ✅ |
| Stage 1 | `make check`、`make fuzz` | 均通过 | ✅ |
| Stage 1 | `make perf-compare` 全绿 | 未跑；脚本只接受 commit，当前工作区未提交 | ⚪ 未测 |
| Stage 1 | Go 1.18/1.27 都跑 race | 仅 1.27.2 | ⚪ 部分 |
| Stage 2 | 8 线程 `Lookup`/`Set` ≥3x base | 分片 64 约 2.4–3.4x | ⚠️ 临界 |
| Stage 2 | 24 线程 ≥4x base | 约 3.2–4.8x（方差大） | ⚠️ 未稳定 |
| Stage 2 | 单线程 `Lookup` 回退 ≤10% | ≈持平 | ✅ |
| Stage 2 | 分片模式单线程 `Set` 回退 ≤70% | 未单独测 | ⚪ 未测 |
| Stage 2 | Zipf 命中率损失 ≤2%，全驻留 ≤3%（16 片） | 1.8% / 原型测量 | ✅（原型） |
| Stage 2 | `Peek` 24 线程 ≥150 Mops/s | 未测（1 分片 10.5ns，理论上限约 95 Mops/s） | ⚪ 未测/大概率未达 |
| Stage 3 | 1000 个未用 TTL 实例 0 goroutine | 0 | ✅ |
| Stage 3 | 仅状态对象 ≤0.5MB | +0.85MB | ❌ |
| Stage 3 | 100K/1M 回收 ≤1.2x 基线 | 以 `Size` 总耗时为代理 6.3 vs 7.9ms | ✅（代理口径） |
| Stage 3 | `Size()` 最大阻塞 <1ms（1M） | review 探针 max 913µs；提交版本未复测 | ⚪ 待复测 |
| Stage 3 | expiry-burst p99 不回退 | 未测 | ⚪ 未测 |

结论：确定性的单线程收益与 API 行为达到目标；**空闲内存明确未达评估报告的原始 Gate；Stage 2 高核数吞吐未稳定达标**。footprint 与 `InsertEvict` 在切到内嵌 link 后均通过（1K 100.0 B、36.5ns）。空闲内存与分片吞吐是设计取舍（每缓存 channel、精确容量切分）的结果；如果门禁是硬性要求，需要懒分配 channel、继续降分片固定成本或重新协商 Gate，而不是在文档里当作通过。

## 3. 取舍与注意事项

1. **内嵌 link 的 64B 布局**：最初把 `prev/next` 直接嵌入 node，当时的 48B 布局让两个 node 共享一条 cache line，8 线程 `Lookup` 从 ~33ns 退到 ~46ns；拆出 24B link 后回到 ~34ns。2026-10-26 的对照原型用 56B node（56→64B size class）复测：1K/100K cache-only 从 108.0/90.9 B 降到 100.0/82.9 B，单线程 `Lookup`/`Set` 快 8–11%，8 线程同序 `Lookup` 36.8→34.5ns，没有复现回退。生产已切回内嵌布局并删掉 `lruLink`/`freeLinks`；关键是窄 value（如 `int`/`uint64`）的 node 要保持 64B size class；若以后加字段或换宽 value 把它推到 72B/80B，需要重新测并发读。见 [embed-link-prototype](2026-10-26-embed-link-prototype.md)。
2. **默认不分片**：分片把全局 LRU 变成每片 LRU，且容量切分在均匀全驻留负载下有约 1.8%（16 片）/5.7%（64 片）的有效容量损失；倾斜负载几乎无损。需要多核吞吐的调用方显式 `Options{Shards: n}`。
3. **预分配默认关闭**：1000 个空缓存如果各自预分配 1024 槽 map 会多占 ~52MB；`Options.Preallocate` 供明确知道会填满的缓存使用。
4. **`Stats` 常开**：计数器在分片锁内自增，实测对 `Lookup`/`Set` 无可见影响；如目标场景对每纳秒敏感，可后续加 `Options.Stats` 开关。
5. **`Size()` 仍是 O(k log n)**：分批只降低最大持锁时间，不降低总工作量；需要 O(1) 监控请用 `Len()`。
6. **高核数方差**：16/24 线程的 run-to-run 波动明显（同一实现 33–44ns），任何 <10% 的结论都应在目标机器上用 AB/BA 复测。
7. **cleaner 唤醒按根 deadline 变化**：`RenewTTL` 若根 deadline 没有改变，不再产生 channel 操作；只有根变化（含清空）才唤醒。分片模式下每片独立比较。
8. **分片哈希带每缓存 seed**：避免固定哈希被构造出单分片热点；未分片路径不计算哈希，也不额外读时钟。
9. **批量清理减少锁往返**：cleaner 每轮一次 `deadlineSnapshot` 同时取最早 deadline 与堆总数，再用纯函数算 batch；不再为 batch 单独扫一遍分片。`checkExpired` 直接复用 `Size` 的分批逻辑。
10. **TTL 写不再串行在 `lifeMu`**：worker 启动后 `ensureCleaner` 用 `workerStarted` 原子快速路径；只有首次启动/关闭竞争才取 cache 级锁。64 分片 24 线程 `SetExpire` 约 32ns，1 分片约 109ns。
11. **淘汰后的内存有上限**：每分片 freelist 最多保留 1024 个 node；空堆容量超过 1024 时释放 backing array。删空或批量过期后不会继续持有峰值 node/堆内存。
12. **`Shards` 真正向下取整**：请求 3/5/6 片时分别得到 2/4/4，和文档一致；容量小于 64 时仍退化为 1 片。

## 4. 复现

```sh
make check
make fuzz FUZZTIME=10s

go test -run '^$' -bench '^BenchmarkCache$' -benchmem \
  -benchtime=250ms -count=3 -cpu=1,8 .

go test -run '^$' -bench '^BenchmarkCacheWorkload/(Hit|Overwrite|InsertEvict|Scan50)-cap1024$' \
  -benchmem -benchtime=250ms -count=3 -cpu=1,8 .

make bench-sharded   # BenchmarkCacheSharded, -cpu=1,2,4,8,24
```

旧版对照可用 `git archive HEAD` 导出到独立目录后执行同样的命令。`make perf-compare` 只接受 commit 中的 runtime 源码，工作区版本无法直接比较；提交后应补跑。

## 5. 未完成与后续

- **提交**：本次改动尚未 commit；提交后再跑 `make perf-compare` 与正式基线。
- **Go 1.18**：本机只有 Go 1.27.2；Go 1.18 实机与 Linux/macOS/Windows CI 矩阵待跑。
- **footprint Gate**：已通过。cache-only 1K 100.0 B、100K 82.9 B（Gate 110）；+keys 口径为 120.9/104.2，引用时需注明口径。
- **空闲内存 Gate**：0.5MB 未达（0.85MB）。可把 `wake`/`stopped` 改为懒分配，但会增加生命周期分支，需单独评估。
- **分片高核数 Gate**：8 线程 3x 为临界（约 2.4–3.4x），24 线程 4x 未稳定（约 3.2–4.8x，方差大）。当前实现优先保证单分片默认语义不变；若要把分片作为主打能力，需要继续降低每片固定成本或改共享日志/无锁读结构。
- **研究型 P2** 仍按评估报告暂缓（淘汰策略、时间轮、字节容量、批量 API 等，见 §1）。
