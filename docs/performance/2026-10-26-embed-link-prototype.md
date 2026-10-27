# 嵌入 LRU link 对照原型：footprint 与并发读权衡

- **日期**：2026-10-26
- **问题**：当前实现把 `prev/next` 放在独立的 `lruLink`（24B）里，目的是避免命中路径的 recency 写与并发读的 key/value 共享缓存行。代价是每 entry 多一次分配、多一次指针跳转。把 link 嵌回 `node` 能否把 footprint 压到 110 B/entry 以下，同时不牺牲 8/24 线程读吞吐？
- **落地状态**：已采纳。生产 `node` 已改为内嵌 `prev/next`（56B），`lruLink`/`freeLinks` 已删除；切回后 `make check`、shuffled race 与 10s fuzz 通过。下面的数据保留为决策依据。
- **结论**：嵌入版每 entry 少 8B（56B 的 node 落入 64B size class，替代 48B node + 24B link），100K 的 cache-only footprint 从 90.9 B 降到 82.9 B，1K 从 108.0 B 降到 100.0 B；单线程 `Lookup`/`Set` 快 9–11%，8 线程同序 `Lookup` 中位数 36.8→34.5ns。**本次测量没有复现“嵌入 link 导致 8 线程读回退”的结论**。原型之后已完成生产切换并通过 `make check`/race/fuzz；正式 ABBA 与 Go 1.18 矩阵仍是后续项。

## 1. 方法

三个变体，除 `node`/link 实现外源码相同，均从当前工作区复制生产文件（`cache_state.go`、`cleaner*.go`、`memcache.go`、`monitor.go`、`node.go`）：

| 变体 | node 布局 | 每 entry 分配 |
|---|---|---:|
| `base` | `cbba0f7` 的 `container/list` + `time.Time` 版本 | — |
| `separate` | 当前生产：node 48B + 独立 pooled `lruLink` 24B | 72B（48 + 24 两个 size class） |
| `embed` | 原型：node 内嵌 `prev/next`，56B | 64B（56 → 64 size class） |

harness 在 `.perf-lab/embed-link/proto_test.go`。`separate/` 是切换前生产源码的快照，`embed/` 由 `gen.py` 从当前生产源码复制生成；整个目录被 `.gitignore` 忽略：

- 机器：AMD Ryzen 9 7900X，24 线程；Go 1.27.2 linux/amd64。
- footprint：填满 `WithLRU[uint64](n, false)`，连续两次 GC 后取 `HeapAlloc` 差值 / n。
  - **cache-only**：key 在采样前预生成，只测缓存自身（与评估报告 `.perf-lab/bench_test.go` 的 `TestFootprint` 同口径）。
  - **+keys**：key 在测量区间内用 `strconv.Itoa` 新建，包含 key 字符串。
- 微基准：1K 热 key 集，`-benchtime=300ms -count=5`，单线程与 `-cpu=8,24`；ABBA 两轮（sep→emb→emb→sep）。
- 并行同序基准：每个 goroutine 都从 key[0] 顺序走，复刻生产 `BenchmarkCache/ParallelLookup` 的访问模式。
- 并行随机基准：每 goroutine 用 LCG 在 1K key 中随机选，降低“所有核同时打同一个 key”的锁竞争。
- 所有变体使用同一把分片锁、同一 `b.RunParallel`、同一 key 集；`InsertEvict`/`RenewTTL` 均为 0 B/op。

## 2. footprint

单位 B/entry；cache-only 数字在 fresh 进程的 3 次重复中稳定在 108.0–108.2（1K）、90.9–91.5（100K，`separate`）；`embed` 为 100.0/82.9。

| 变体 | 1K cache-only | 100K cache-only | 1K +keys | 100K +keys |
|---|---:|---:|---:|---:|
| `base`（cbba0f7） | 147.8 | 130.9 | 168.7 | 152.2 |
| `separate`（当前） | **108.0** | **90.9** | 129.0 | 112.2 |
| `embed`（原型） | **100.0** | **82.9** | 120.9 | 104.2 |

解读：

1. 110 B/entry 的 Gate 是否通过，**很大程度上取决于是否把新建 key 字符串算进去**：
   - 按评估报告自己的 `TestFootprint` 口径（key 预生成、cache-only），当前 `separate` 在 fresh 进程的 1K 是 108.0（3 次 108.0–108.2），但同一测试放在全套件里重复时曾到 **113.4**；110 的 Gate 落在分配器/GC 噪声边缘，不能算稳定通过。`embed` 的 100.0 才把余量扩大到约 10B。
   - 若把每个 key 字符串也算进缓存成本（1K 时约 21 B/entry），三者都不通过，`embed` 也只到 120.9。这个口径更接近“调用方只给 key、缓存负责存 key”的真实成本，但不是原报告的口径。
2. `embed` 的 8B 节省不是来自“少 16B 原始字段”，而是 allocator：node 从 48B（48 size class）+ 24B link（24 class）= 72B，变成 56B（64 size class）= 64B。原始字段确实少 16B，但 56 被抬到 64，最终只省 8B。
3. 1K 的单位成本高于 100K，是 map 桶、shard 结构、key 长度摊销的结果，不是 entry 本身变大。

## 3. 单线程

中位数（5 次），单位 ns/op，均 0 alloc：

| 操作 | `separate` | `embed` | 变化 |
|---|---:|---:|---:|
| `Lookup` 命中 | 16.34 | **14.96** | −8.4% |
| `Set` 覆盖 | 18.09 | **16.10** | −11.0% |
| `InsertEvict` | 46.52 | **45.21** | −2.8% |
| `RenewTTL` | 91.75 | **88.59** | −3.4% |

`Lookup`/`Set` 的收益主要来自少一次 `n.link` 间接寻址和少一次 link 对象分配/回收；`InsertEvict`/`RenewTTL` 被堆和 map 操作摊薄，差异较小。

## 4. 并发读

### 4.1 同序访问（生产 `ParallelLookup` 模式）

10 次采样（ABBA 两轮 × 5）的中位数与范围：

| 线程 | `separate` | `embed` |
|---|---:|---:|
| 8 | 36.8ns（33.5–45.1） | **34.5ns（32.8–38.9）** |
| 24 | 66.0ns（55.3–102.2） | **61.0ns（54.9–90.7）** |

24 线程的 run-to-run 方差很大（同一实现能到 100ns），**不能据此声称 8% 的改善**；能说的是：本次没有看到嵌入版一致变慢，8 线程中位数反而略低。

### 4.2 随机/混合访问

单轮 5 次的中位数：

| 基准 | 线程 | `separate` | `embed` |
|---|---:|---:|---:|
| 随机 `Lookup` | 8 | 42.1 | **38.3** |
| 随机 `Lookup` | 24 | 86.0 | **73.8** |
| 95% 读 + 5% 写 | 8 | 44.4 | **39.5** |
| 95% 读 + 5% 写 | 24 | 74.2 | **62.2** |
| `Peek`（无 recency 写） | 8 | 23.7 | 22.7 |
| `Peek` | 24 | 43.8 | **39.8** |

随机/混合负载下嵌入版也略快，且 `Peek` 两者接近（`Peek` 不写 link，符合预期）。

## 5. 为什么和之前的结论相反

实施记录里写过“把 `prev/next` 嵌回 node 后，8 线程 `Lookup` 从 ~33ns 退到 ~46ns”。这次没有复现，最可能的差别是当时的嵌入布局使 node 落在 48B size class，两个 node 共享一条 64B cache line，写一个 node 的 link 会失效邻居的 key/value 行；当前原型 node 是 56B，进入 64B size class，基本一个 node 占一条 line，false sharing 消失。另外 24 线程的噪声本身就足以制造 33→46 的读数。

因此“必须把 link 拆出去”不是普遍结论，而是**特定 node 大小/对齐下的结论**。如果采用嵌入布局，应把 node 大小固定在 64B size class，并加一个布局断言或 footprint 测试防止以后加字段把它推到 72B/80B 而重新引入 false sharing。

## 6. 决策与后续

- **已切换**：生产 `node` 采用内嵌 `prev/next`，删除了 `lruLink`/`freeLinks`、每 entry 少 8B，单线程 `Lookup`/`Set` 快 8–11%。切换的直接收益不只是 footprint（旧布局 fresh 108 名义上已过 110），而是 10B 的 Gate 余量、更简单的代码和更少的指针跳转。
- **已完成的验证**：`make check`（gofmt、golangci-lint v2.15.0、vet、单测、3 次 shuffled race）、`make fuzz FUZZTIME=10s`、`BenchmarkCache`/`BenchmarkCacheWorkload` 复测。
- **仍需补**：
  1. 正式 ABBA 跑 `BenchmarkCache`/`BenchmarkCacheSharded`/`BenchmarkCacheWorkload`，尤其是 8/24 线程；
  2. Go 1.18 实机与 CI 矩阵，确认 size class 与 map 开销下 1K 仍 ≤110；
  3. 提交后跑 `make perf-compare`。
- **布局约束**：node 必须保持 56B/64B size class；若以后加字段把它推到 72B/80B，需要重新测并发读与 footprint，避免重新引入 false sharing。

## 7. 复现

```sh
# 生产已内嵌；gen.py 重建 embed/，separate/ 保留为切换前快照
python3 .perf-lab/embed-link/gen.py

# footprint（cache-only 与 +keys）
(cd .perf-lab/embed-link/separate && go test -run TestProtoFootprint -v -count=3)
(cd .perf-lab/embed-link/embed    && go test -run TestProtoFootprint -v -count=3)

# 单线程
(cd .perf-lab/embed-link/separate && go test -run '^$' \
  -bench 'BenchmarkProto(Lookup|Set|InsertEvict|RenewTTL)$' -benchmem -count=5)
(cd .perf-lab/embed-link/embed && go test -run '^$' \
  -bench 'BenchmarkProto(Lookup|Set|InsertEvict|RenewTTL)$' -benchmem -count=5)

# 并行 ABBA（sep → emb → emb → sep）
(cd .perf-lab/embed-link/separate && go test -run '^$' \
  -bench BenchmarkProtoParallelSequential -benchmem -count=5 -cpu=8,24)
(cd .perf-lab/embed-link/embed && go test -run '^$' \
  -bench BenchmarkProtoParallelSequential -benchmem -count=5 -cpu=8,24)
```

原始输出在同目录 `out-*.txt`、`seq-*.txt`、`single-*.txt`；`base` 变体由 `git archive cbba0f7` 生成，只保留能编译的 footprint harness。
